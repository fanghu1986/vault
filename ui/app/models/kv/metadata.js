/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Model, { attr } from '@ember-data/model';
import { withModelValidations } from 'vault/decorators/model-validations';
import { withFormFields } from 'vault/decorators/model-form-fields';

const validations = {
  maxVersions: [
    { type: 'number', message: 'Maximum versions must be a number.' },
    { type: 'length', options: { min: 1, max: 16 }, message: 'You cannot go over 16 characters.' },
  ],
};
const formFieldProps = ['maxVersions', 'casRequired', 'deleteVersionAfter', 'customerMetadata'];

@withModelValidations(validations)
@withFormFields(formFieldProps)
export default class KvSecretMetadataModel extends Model {
  @attr('string') backend; // dynamic path of secret -- set on response from value passed to queryRecord.
  @attr('string') path;

  @attr('number', {
    defaultValue: 0,
    label: 'Maximum number of versions',
    subText:
      'The number of versions to keep per key. Once the number of keys exceeds the maximum number set here, the oldest version will be permanently deleted.',
  })
  maxVersions;

  @attr('boolean', {
    defaultValue: false,
    label: 'Require Check and Set',
    subText: `Writes will only be allowed if the key's current version matches the version specified in the cas parameter.`,
  })
  casRequired;

  @attr('string', {
    defaultValue: '0s',
    editType: 'ttl',
    label: 'Automate secret deletion',
    helperTextDisabled: `A secret's version must be manually deleted.`,
    helperTextEnabled: 'Delete all new versions of this secret after.',
  })
  deleteVersionAfter;

  @attr('object', {
    editType: 'kv',
    subText: 'An optional set of informational key-value pairs that will be stored with all secret versions.',
  })
  customMetadata;

  // Additional Params only returned on the GET response.
  @attr('string') createdTime;
  @attr('number') currentVersion;
  @attr('number') oldestVersion;
  @attr('string') updatedTime;
  @attr('object') versions;
}
