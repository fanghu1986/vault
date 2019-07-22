package pcf

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/vault-plugin-auth-pcf/models"
	"github.com/hashicorp/vault-plugin-auth-pcf/signatures"
	"github.com/hashicorp/vault-plugin-auth-pcf/util"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/cidrutil"
	"github.com/hashicorp/vault/sdk/helper/strutil"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/pkg/errors"
)

func (b *backend) pathLogin() *framework.Path {
	return &framework.Path{
		Pattern: "login",
		Fields: map[string]*framework.FieldSchema{
			"role": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name:  "Role Name",
					Value: "internally-defined-role",
				},
				Description: "The name of the role to authenticate against.",
			},
			"cf_instance_cert": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "CF_INSTANCE_CERT Contents",
				},
				Description: "The full body of the file available at the CF_INSTANCE_CERT path on the PCF instance.",
			},
			"signing_time": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name:  "Signing Time",
					Value: "2006-01-02T15:04:05Z",
				},
				Description: "The date and time used to construct the signature.",
			},
			"signature": {
				Required: true,
				Type:     framework.TypeString,
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "Signature",
				},
				Description: "The signature generated by the client certificate's private key.",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.operationLoginUpdate,
			},
		},
		HelpSynopsis:    pathLoginSyn,
		HelpDescription: pathLoginDesc,
	}
}

// operationLoginUpdate is called by those wanting to gain access to Vault.
// They present the instance certificates that should have been issued by the pre-configured
// Certificate Authority, and a signature that should have been signed by the instance cert's
// private key. If this holds true, there are additional checks verifying everything looks
// good before authentication is given.
func (b *backend) operationLoginUpdate(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	// Grab the time immediately for checking against the request's signingTime.
	timeReceived := time.Now().UTC()

	roleName := data.Get("role").(string)
	if roleName == "" {
		return logical.ErrorResponse("'role-name' is required"), nil
	}

	// Ensure the pcf certificate meets the role's constraints.
	role, err := getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errors.New("no matching role")
	}

	if len(role.TokenBoundCIDRs) > 0 {
		if req.Connection == nil {
			b.Logger().Warn("token bound CIDRs found but no connection information available for validation")
			return nil, logical.ErrPermissionDenied
		}
		if !cidrutil.RemoteAddrIsOk(req.Connection.RemoteAddr, role.TokenBoundCIDRs) {
			return nil, logical.ErrPermissionDenied
		}
	}

	signature := data.Get("signature").(string)
	if signature == "" {
		return logical.ErrorResponse("'signature' is required"), nil
	}

	cfInstanceCertContents := data.Get("cf_instance_cert").(string)
	if cfInstanceCertContents == "" {
		return logical.ErrorResponse("'cf_instance_cert' is required"), nil
	}

	signingTimeRaw := data.Get("signing_time").(string)
	if signingTimeRaw == "" {
		return logical.ErrorResponse("'signing_time' is required"), nil
	}
	signingTime, err := parseTime(signingTimeRaw)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	config, err := config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("no CA is configured for verifying client certificates")
	}

	// Ensure the time it was signed isn't too far in the past or future.
	oldestAllowableSigningTime := timeReceived.Add(-1 * config.LoginMaxSecOld)
	furthestFutureAllowableSigningTime := timeReceived.Add(config.LoginMaxSecAhead)
	if signingTime.Before(oldestAllowableSigningTime) {
		return logical.ErrorResponse(fmt.Sprintf("request is too old; signed at %s but received request at %s; allowable seconds old is %d", signingTime, timeReceived, config.LoginMaxSecOld/time.Second)), nil
	}
	if signingTime.After(furthestFutureAllowableSigningTime) {
		return logical.ErrorResponse(fmt.Sprintf("request is too far in the future; signed at %s but received request at %s; allowable seconds in the future is %d", signingTime, timeReceived, config.LoginMaxSecAhead/time.Second)), nil
	}

	intermediateCert, identityCert, err := util.ExtractCertificates(cfInstanceCertContents)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	// Ensure the private key used to create the signature matches our identity
	// certificate, and that it signed the same data as is presented in the body.
	// This offers some protection against MITM attacks.
	signingCert, err := signatures.Verify(signature, &signatures.SignatureData{
		SigningTime:            signingTime,
		Role:                   roleName,
		CFInstanceCertContents: cfInstanceCertContents,
	})
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	// Make sure the identity/signing cert was actually issued by our CA.
	if err := util.Validate(config.IdentityCACertificates, intermediateCert, identityCert, signingCert); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	// Read PCF's identity fields from the certificate.
	pcfCert, err := models.NewPCFCertificateFromx509(signingCert)
	if err != nil {
		return nil, err
	}

	// It may help some users to be able to easily view the incoming certificate information
	// in an un-encoded format, as opposed to the encoded format that will appear in the Vault
	// audit logs.
	if b.Logger().IsDebug() {
		b.Logger().Debug(fmt.Sprintf("handling login attempt from %+v", pcfCert))
	}

	if err := b.validate(config, role, pcfCert, req.Connection.RemoteAddr); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	// Everything checks out.
	auth := &logical.Auth{
		InternalData: map[string]interface{}{
			"role":        roleName,
			"instance_id": pcfCert.InstanceID,
			"ip_address":  pcfCert.IPAddress.String(),
		},
		DisplayName: pcfCert.InstanceID,
		Alias: &logical.Alias{
			Name: pcfCert.AppID,
			Metadata: map[string]string{
				"org_id":   pcfCert.OrgID,
				"app_id":   pcfCert.AppID,
				"space_id": pcfCert.SpaceID,
			},
		},
	}

	role.PopulateTokenAuth(auth)

	return &logical.Response{
		Auth: auth,
	}, nil
}

func (b *backend) pathLoginRenew(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	config, err := config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("no configuration is available for reaching the PCF API")
	}

	roleName, err := getOrErr("role", req.Auth.InternalData)
	if err != nil {
		return nil, err
	}

	role, err := getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errors.New("no matching role")
	}

	instanceID, err := getOrErr("instance_id", req.Auth.InternalData)
	if err != nil {
		return nil, err
	}

	ipAddr, err := getOrErr("ip_address", req.Auth.InternalData)
	if err != nil {
		return nil, err
	}

	orgID, err := getOrErr("org_id", req.Auth.Alias.Metadata)
	if err != nil {
		return nil, err
	}

	spaceID, err := getOrErr("space_id", req.Auth.Alias.Metadata)
	if err != nil {
		return nil, err
	}

	appID, err := getOrErr("app_id", req.Auth.Alias.Metadata)
	if err != nil {
		return nil, err
	}

	// Reconstruct the certificate and ensure it still meets all constraints.
	pcfCert, err := models.NewPCFCertificate(instanceID, orgID, spaceID, appID, ipAddr)
	if err := b.validate(config, role, pcfCert, req.Connection.RemoteAddr); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	resp := &logical.Response{Auth: req.Auth}
	resp.Auth.TTL = role.TokenTTL
	resp.Auth.MaxTTL = role.TokenMaxTTL
	resp.Auth.Period = role.TokenPeriod
	return resp, nil
}

func (b *backend) validate(config *models.Configuration, role *models.RoleEntry, pcfCert *models.PCFCertificate, reqConnRemoteAddr string) error {
	if !role.DisableIPMatching {
		if !matchesIPAddress(reqConnRemoteAddr, pcfCert.IPAddress) {
			return errors.New("no matching IP address")
		}
	}
	if !meetsBoundConstraints(pcfCert.InstanceID, role.BoundInstanceIDs) {
		return fmt.Errorf("instance ID %s doesn't match role constraints of %s", pcfCert.InstanceID, role.BoundInstanceIDs)
	}
	if !meetsBoundConstraints(pcfCert.AppID, role.BoundAppIDs) {
		return fmt.Errorf("app ID %s doesn't match role constraints of %s", pcfCert.AppID, role.BoundAppIDs)
	}
	if !meetsBoundConstraints(pcfCert.OrgID, role.BoundOrgIDs) {
		return fmt.Errorf("org ID %s doesn't match role constraints of %s", pcfCert.OrgID, role.BoundOrgIDs)
	}
	if !meetsBoundConstraints(pcfCert.SpaceID, role.BoundSpaceIDs) {
		return fmt.Errorf("space ID %s doesn't match role constraints of %s", pcfCert.SpaceID, role.BoundSpaceIDs)
	}
	// Use the PCF API to ensure everything still exists and to verify whatever we can.
	client, err := util.NewPCFClient(config)
	if err != nil {
		return err
	}

	// Here, if it were possible, we _would_ do an API call to check the instance ID,
	// but currently there's no known way to do that via the pcf API.

	// Check everything we can using the app ID.
	app, err := client.AppByGuid(pcfCert.AppID)
	if err != nil {
		return err
	}
	if app.Guid != pcfCert.AppID {
		return fmt.Errorf("cert app ID %s doesn't match API's expected one of %s", pcfCert.AppID, app.Guid)
	}
	if app.SpaceGuid != pcfCert.SpaceID {
		return fmt.Errorf("cert space ID %s doesn't match API's expected one of %s", pcfCert.SpaceID, app.SpaceGuid)
	}
	if app.Instances <= 0 {
		return errors.New("app doesn't have any live instances")
	}

	// Check everything we can using the org ID.
	org, err := client.GetOrgByGuid(pcfCert.OrgID)
	if err != nil {
		return err
	}
	if org.Guid != pcfCert.OrgID {
		return fmt.Errorf("cert org ID %s doesn't match API's expected one of %s", pcfCert.OrgID, org.Guid)
	}

	// Check everything we can using the space ID.
	space, err := client.GetSpaceByGuid(pcfCert.SpaceID)
	if err != nil {
		return err
	}
	if space.Guid != pcfCert.SpaceID {
		return fmt.Errorf("cert space ID %s doesn't match API's expected one of %s", pcfCert.SpaceID, space.Guid)
	}
	if space.OrganizationGuid != pcfCert.OrgID {
		return fmt.Errorf("cert org ID %s doesn't match API's expected one of %s", pcfCert.OrgID, space.OrganizationGuid)
	}
	return nil
}

func meetsBoundConstraints(certValue string, constraints []string) bool {
	if len(constraints) == 0 {
		// There are no restrictions, so everything passes this check.
		return true
	}
	// Check whether we have a match.
	return strutil.StrListContains(constraints, certValue)
}

func matchesIPAddress(remoteAddr string, certIP net.IP) bool {
	// Some remote addresses may arrive like "10.255.181.105/32"
	// but the certificate will only have the IP address without
	// the subnet mask, so that's what we want to match against.
	// For those wanting to also match the subnet, use bound_cidrs.
	parts := strings.Split(remoteAddr, "/")
	reqIPAddr := net.ParseIP(parts[0])
	if certIP.Equal(reqIPAddr) {
		return true
	}
	return false
}

// Try parsing this as ISO 8601 AND the way that is default provided by Bash to make it easier to give via the CLI as well.
func parseTime(signingTime string) (time.Time, error) {
	if signingTime, err := time.Parse(signatures.TimeFormat, signingTime); err == nil {
		return signingTime, nil
	}
	if signingTime, err := time.Parse(util.BashTimeFormat, signingTime); err == nil {
		return signingTime, nil
	}
	return time.Time{}, fmt.Errorf("couldn't parse %s", signingTime)
}

// getOrErr is a convenience method for pulling a string from a map.
func getOrErr(fieldName string, from interface{}) (string, error) {
	switch givenMap := from.(type) {
	case map[string]interface{}:
		vIfc, ok := givenMap[fieldName]
		if !ok {
			return "", fmt.Errorf("unable to retrieve %q during renewal", fieldName)
		}
		v, ok := vIfc.(string)
		if v == "" {
			return "", fmt.Errorf("unable to retrieve %q during renewal, not a string", fieldName)
		}
		return v, nil
	case map[string]string:
		v, ok := givenMap[fieldName]
		if !ok {
			return "", fmt.Errorf("unable to retrieve %q during renewal", fieldName)
		}
		return v, nil
	default:
		return "", fmt.Errorf("unrecognized type for structure containing %s", fieldName)
	}
}

const pathLoginSyn = `
Authenticates an entity with Vault.
`

const pathLoginDesc = `
Authenticate PCF entities using a client certificate issued by the 
configured Certificate Authority, and signed by a client key belonging
to the client certificate.
`
