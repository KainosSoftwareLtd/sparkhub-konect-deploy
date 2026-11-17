// The managed identity as the PostgreSQL server's Entra administrator. Its Postgres role is principalName (the identity's
// name), which Konect signs in as with a token for https://ossrdbms-aad.database.windows.net/.default.
// A module because the entry is named by the identity's object id, which exists only once the identity does.

@description('The PostgreSQL Flexible Server (in this resource group).')
param serverName string

@description('Object id of the identity.')
param principalId string

@description('The identity\'s name: the Postgres role name.')
param principalName string

@description('Entra tenant of the identity.')
param tenantId string

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2026-10-01' existing = {
  name: serverName
}

resource admin 'Microsoft.DBforPostgreSQL/flexibleServers/administrators@2026-10-01' = {
  parent: server
  name: principalId
  properties: {
    principalType: 'ServicePrincipal'
    principalName: principalName
    tenantId: tenantId
  }
}
