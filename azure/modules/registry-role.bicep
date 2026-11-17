// AcrPull for the identity on a private Azure Container Registry (registryId), at the registry's scope. A module so the
// registry can live in another resource group or subscription.

@description('The registry.')
param registryName string

@description('Resource ID of the user-assigned identity (part of the assignment\'s stable name).')
param identityId string

@description('Object id of the identity.')
param principalId string

// AcrPull
var acrPull = '7f951dda-4ed3-4680-a7ca-43fe172d538d'

resource registry 'Microsoft.ContainerRegistry/registries@2025-04-01' existing = {
  name: registryName
}

resource assignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, identityId, acrPull)
  scope: registry
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', acrPull)
    principalId: principalId
    // Set, so the assignment does not wait for the new identity to replicate in Entra.
    principalType: 'ServicePrincipal'
  }
}
