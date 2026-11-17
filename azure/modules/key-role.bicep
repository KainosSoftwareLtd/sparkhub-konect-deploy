// The identity's role on the wrapping key, at the key's scope (covers every version of the key). A module so that it can
// target the old deployment's key in another resource group (a restore with existingKeyId).

@description('Vault holding the key.')
param vaultName string

@description('The key.')
param keyName string

@description('Resource ID of the user-assigned identity (part of the assignment\'s stable name).')
param identityId string

@description('Object id of the identity.')
param principalId string

@description('Built-in role definition GUID (Key Vault Crypto Service Encryption User).')
param roleDefinitionGuid string

resource vault 'Microsoft.KeyVault/vaults@2026-05-15' existing = {
  name: vaultName
}

resource key 'Microsoft.KeyVault/vaults/keys@2026-05-15' existing = {
  parent: vault
  name: keyName
}

resource assignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(key.id, identityId, roleDefinitionGuid)
  scope: key
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', roleDefinitionGuid)
    principalId: principalId
    // Set, so the assignment does not wait for the new identity to replicate in Entra.
    principalType: 'ServicePrincipal'
  }
}
