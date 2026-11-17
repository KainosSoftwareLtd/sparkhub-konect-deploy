// Konect on Azure: one Container App (the public bootstrap image, or your strict image), PostgreSQL Flexible Server
// signed in with the app's managed identity (Entra only, no password anywhere), and a Key Vault key that wraps
// Konect's local key half (localKey source `azurekv`). TLS at the Container Apps ingress.
//
// Source of truth. `azuredeploy.json` beside it is built from this file (`scripts/build-azure.sh`) and CI fails when it
// is stale. The Deploy to Azure button and `scripts/azure.sh` both deploy that JSON, so they produce the same thing.

metadata description = 'SparkHub Konect on Azure Container Apps, PostgreSQL Flexible Server (Entra sign-in with the managed identity) and a Key Vault key. Takes about 20 minutes, most of it the Container Apps environment.'

targetScope = 'resourceGroup'

// ── Parameters ───────────────────────────────────────────────────────────────

@description('Name prefix of every resource: 3 to 13 lowercase letters, digits or hyphens, starting with a letter. Keep it the same for every redeploy (updates find the resources by it).')
@minLength(3)
@maxLength(13)
param name string = 'konect'

@description('Region. Default: the resource group\'s region.')
param location string = resourceGroup().location

@description('The Konect version to run, exactly X.Y.Z (X.Y.Z-rc.N against SparkHub preprod only). Change it and redeploy to upgrade or roll back.')
@minLength(5)
@maxLength(40)
param konectVersion string

@description('bootstrap (default): the public bootstrap image downloads and verifies Konect at start, with the install token. strict: your own image built with strict/Dockerfile; no token is stored anywhere.')
@allowed([
  'bootstrap'
  'strict'
])
param imageKind string = 'bootstrap'

@description('Container image, pinned by digest. Default: the bootstrap image this template release was tested with. strict: your image (for example myregistry.azurecr.io/konect@sha256:...).')
param image string = 'ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap@sha256:bbd47e455e4463aa013052bf47409bee9e99ec418c5314607565328dcf9b4903'

@description('Private registry only: the resource ID of the Azure Container Registry that holds image (/subscriptions/<id>/resourceGroups/<rg>/providers/Microsoft.ContainerRegistry/registries/<name>). The managed identity pulls from it; createRoleAssignments=true grants it AcrPull on the registry. Empty: a public image (the default bootstrap image needs none).')
param registryId string = ''

@description('The install token from SparkHub (Konect, Install; starts with kit_). Required with imageKind bootstrap; must be entered again on every redeploy. Stored only as a Container Apps secret.')
@secure()
param installToken string = ''

@description('Which SparkHub this Konect belongs to.')
@allowed([
  'prod'
  'preprod'
])
param sparkhub string = 'prod'

@description('Public host name if you will bind your own (binding the custom domain is a step after the deploy). Empty: the generated https://<name>.<environment domain>.')
param customDomain string = ''

@description('true: the template grants the managed identity wrap/unwrap on the key (needs Owner, User Access Administrator or Role Based Access Control Administrator). false: Contributor is enough; a platform team then runs the one role assignment shown in the nextStep output.')
param createRoleAssignments bool = true

@description('PostgreSQL Flexible Server SKU.')
param postgresSku string = 'Standard_B1ms'

@description('PostgreSQL Flexible Server tier (must match the SKU).')
@allowed([
  'Burstable'
  'GeneralPurpose'
  'MemoryOptimized'
])
param postgresTier string = 'Burstable'

@description('PostgreSQL storage in GB.')
@minValue(32)
param postgresStorageGb int = 32

@description('vCPU of the container (Consumption sizes: 0.25 to 4, paired with memory).')
param cpu string = '0.5'

@description('Memory of the container (twice the vCPU in Gi, for example 1Gi for 0.5).')
param memory string = '1Gi'

@description('Key Vault purge protection (90-day soft delete). false only for test deployments you will delete (7-day soft delete, purgeable). Cannot be switched off once on.')
param purgeProtection bool = true

@description('Restore only: the resource ID of the key the old deployment used (its keyId output). No new vault or key is created; the identity is granted on this key.')
param existingKeyId string = ''

@description('Restore only: the name of a PostgreSQL Flexible Server in this resource group (a point-in-time restore of the old one) to use instead of creating one.')
param postgresServerName string = ''

// ── Input checks ─────────────────────────────────────────────────────────────
// ARM has no regular expressions and no assertion that stops a deployment with a message, so the checks build one
// message and, when it is not empty, read a property of that name from an empty object. ARM then fails the deployment
// in validation with "The language expression property '<message>' doesn't exist", before anything is created.
// (`if` evaluates only the branch it takes.)

var lowerChars = 'abcdefghijklmnopqrstuvwxyz'
var nameOk = contains(lowerChars, substring(name, 0, 1)) && !endsWith(name, '-') && empty(filter(range(0, length(name)), i => !contains('${lowerChars}0123456789-', substring(name, i, 1))))
// konectVersion: X.Y.Z or X.Y.Z-rc.N, digits only in each number.
var versionSplit = split(konectVersion, '-')
var versionCore = split(versionSplit[0], '.')
var versionCoreOk = length(versionCore) == 3 && empty(filter(versionCore, p => empty(p))) && empty(filter(range(0, length(versionSplit[0])), i => !contains('0123456789.', substring(versionSplit[0], i, 1))))
var versionRc = length(versionSplit) == 2 ? versionSplit[1] : ''
var versionRcOk = length(versionSplit) == 1 || (length(versionSplit) == 2 && startsWith(versionRc, 'rc.') && length(versionRc) > 3 && empty(filter(range(3, max(length(versionRc) - 3, 0)), i => !contains('0123456789', substring(versionRc, i, 1)))))
var keyIdParts = split(existingKeyId, '/')
// Ternaries, not && / ||: only `if` is known to evaluate just the branch it takes (keyIdParts[9] needs 11 parts).
var keyIdOk = empty(existingKeyId)
  ? true
  : length(keyIdParts) == 11
      ? toLower(join(take(skip(keyIdParts, 5), 3), '/')) == 'providers/microsoft.keyvault/vaults' && toLower(keyIdParts[9]) == 'keys'
      : false
var registryParts = split(registryId, '/')
var registryOk = empty(registryId)
  ? true
  : length(registryParts) == 9
      ? toLower(join(take(skip(registryParts, 5), 3), '/')) == 'providers/microsoft.containerregistry/registries'
      : false

var inputError = !nameOk
  ? 'name must be lowercase letters, digits or hyphens, starting with a letter'
  : !versionCoreOk || !versionRcOk
      ? 'konectVersion must be X.Y.Z or X.Y.Z-rc.N, for example 0.7.0 (no v, no latest)'
      : imageKind == 'bootstrap' && empty(installToken)
          ? 'installToken is required with imageKind bootstrap (SparkHub, Konect, Install, Create token)'
          : !empty(installToken) && !startsWith(installToken, 'kit_')
              ? 'installToken must start with kit_'
              : empty(image)
                  ? 'image is required (the bootstrap image pinned by digest, or your strict image)'
                  : !keyIdOk
                      ? 'existingKeyId must be a key resource ID: /subscriptions/<id>/resourceGroups/<rg>/providers/Microsoft.KeyVault/vaults/<vault>/keys/<key>'
                      : !registryOk
                          ? 'registryId must be a container registry resource ID: /subscriptions/<id>/resourceGroups/<rg>/providers/Microsoft.ContainerRegistry/registries/<name>'
                          : ''
var checkedVersion = empty(inputError) ? konectVersion : string(any(json('{}'))[inputError])

// ── Names ────────────────────────────────────────────────────────────────────

var suffix = uniqueString(resourceGroup().id, name)
var bootstrap = imageKind == 'bootstrap'
var createKey = empty(existingKeyId)
var vaultName = 'kv${suffix}'
var serverName = empty(postgresServerName) ? '${name}-pg-${suffix}' : postgresServerName
var keySubscription = createKey ? subscription().subscriptionId : keyIdParts[2]
var keyResourceGroup = createKey ? resourceGroup().name : keyIdParts[4]
var keyVaultName = createKey ? vaultName : keyIdParts[8]
var keyName = createKey ? 'konect-local-half' : keyIdParts[10]
var keyResourceId = resourceId(keySubscription, keyResourceGroup, 'Microsoft.KeyVault/vaults/keys', keyVaultName, keyName)
// The versionless key URI: Konect records the version it wraps with, so a new key version never breaks a store.
var keyUri = 'https://${keyVaultName}${environment().suffixes.keyvaultDns}/keys/${keyName}'
var cryptoServiceEncryptionUser = 'e147488a-f6f5-4113-8e2d-b22465e65bf6'
var useRegistry = !empty(registryId)
// Ternaries: registryParts[8] exists only for a valid registryId.
var registryName = useRegistry && registryOk ? registryParts[8] : ''
var registryServer = '${registryName}${environment().suffixes.acrLoginServer}'
var tags = {
  app: 'konect'
}

// ── Resources ────────────────────────────────────────────────────────────────

resource logs 'Microsoft.OperationalInsights/workspaces@2026-03-01' = {
  name: '${name}-logs'
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
  }
}

resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30' = {
  name: '${name}-id'
  location: location
  // The version tag carries the input checks: it is evaluated in validation, before anything is created.
  tags: union(tags, {
    'konect-version': checkedVersion
  })
}

resource vault 'Microsoft.KeyVault/vaults@2026-05-15' = if (createKey) {
  name: vaultName
  location: location
  tags: tags
  properties: {
    tenantId: tenant().tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: purgeProtection ? 90 : 7
    // Key Vault refuses an explicit false: leave it unset when off.
    enablePurgeProtection: purgeProtection ? true : null
    publicNetworkAccess: 'Enabled'
  }
}

// L's wrapping key. No rotation policy: a new version is created only on purpose, and old versions stay usable.
resource key 'Microsoft.KeyVault/vaults/keys@2026-05-15' = if (createKey) {
  parent: vault
  name: 'konect-local-half'
  tags: tags
  properties: {
    kty: 'RSA'
    keySize: 3072
    keyOps: [
      'wrapKey'
      'unwrapKey'
    ]
    attributes: {
      enabled: true
      exportable: false
    }
  }
}

// Key Vault Crypto Service Encryption User (keys/read, wrap, unwrap) at the key's scope: every version of this key.
module keyRole 'modules/key-role.bicep' = if (createRoleAssignments) {
  name: 'konect-key-role-${suffix}'
  scope: resourceGroup(keySubscription, keyResourceGroup)
  params: {
    vaultName: keyVaultName
    keyName: keyName
    identityId: identity.id
    principalId: identity.properties.principalId
    roleDefinitionGuid: cryptoServiceEncryptionUser
  }
  dependsOn: [
    key
  ]
}

// AcrPull on a private registry (registryId). Before the app: Container Apps pulls the image when it creates revision 1.
module registryRole 'modules/registry-role.bicep' = if (useRegistry && createRoleAssignments) {
  name: 'konect-registry-role-${suffix}'
  scope: resourceGroup(useRegistry ? registryParts[2] : subscription().subscriptionId, useRegistry ? registryParts[4] : resourceGroup().name)
  params: {
    registryName: registryName
    identityId: identity.id
    principalId: identity.properties.principalId
  }
}

resource postgres 'Microsoft.DBforPostgreSQL/flexibleServers@2026-10-01' = if (empty(postgresServerName)) {
  name: serverName
  location: location
  tags: tags
  sku: {
    name: postgresSku
    tier: postgresTier
  }
  properties: {
    version: '16'
    // Entra only. No administratorLogin: it is needed only when password auth is first enabled.
    authConfig: {
      activeDirectoryAuth: 'Enabled'
      passwordAuth: 'Disabled'
      tenantId: tenant().tenantId
    }
    storage: {
      storageSizeGB: postgresStorageGb
    }
    backup: {
      backupRetentionDays: 7
      geoRedundantBackup: 'Disabled'
    }
    highAvailability: {
      mode: 'Disabled'
    }
    network: {
      publicNetworkAccess: 'Enabled'
    }
  }
}

// The server this deployment uses: the new one, or the restored one named by postgresServerName.
resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2026-10-01' existing = {
  name: serverName
}

// A flexible server runs one operation at a time, so its children go one after the other:
// database, then firewall rule, then the Entra administrator.
resource database 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2026-10-01' = {
  parent: server
  name: 'konect'
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
  dependsOn: [
    postgres
  ]
}

// Azure services only (0.0.0.0), as the scripts did. Private networking is later hardening.
resource firewall 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2026-10-01' = {
  parent: server
  name: 'AllowAllAzureServicesAndResourcesWithinAzureIps'
  properties: {
    startIpAddress: '0.0.0.0'
    endIpAddress: '0.0.0.0'
  }
  dependsOn: [
    database
  ]
}

// The identity is the server's Entra administrator (azure_pg_admin: CREATEROLE, CREATEDB, not superuser). Its
// Postgres role is the identity's name, which Konect signs in as (KONECT_PG_USER). In a module because the entry's
// name is the identity's object id, known only once the identity exists.
module postgresAdmin 'modules/postgres-admin.bicep' = {
  name: 'konect-pg-admin-${suffix}'
  params: {
    serverName: serverName
    principalId: identity.properties.principalId
    principalName: identity.name
    tenantId: tenant().tenantId
  }
  dependsOn: [
    firewall
  ]
}

resource containerEnv 'Microsoft.App/managedEnvironments@2026-07-01' = {
  name: '${name}-env'
  location: location
  tags: tags
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logs.properties.customerId
        sharedKey: logs.listKeys().primarySharedKey
      }
    }
    workloadProfiles: [
      {
        name: 'Consumption'
        workloadProfileType: 'Consumption'
      }
    ]
  }
}

var baseUrl = empty(customDomain) ? 'https://${name}.${containerEnv.properties.defaultDomain}' : 'https://${customDomain}'
var health = {
  path: '/v1/health'
  port: 41800
  scheme: 'HTTP'
}
var konectEnv = [
  {
    name: 'KONECT_VERSION'
    value: checkedVersion
  }
  {
    name: 'KONECT_SPARKHUB'
    value: sparkhub
  }
  {
    name: 'KONECT_CONFIG'
    value: 'builtin:postgres-azure'
  }
  {
    name: 'KONECT_BASE_URL'
    value: baseUrl
  }
  {
    name: 'KONECT_PG_HOST'
    value: server.properties.fullyQualifiedDomainName
  }
  {
    name: 'KONECT_PG_USER'
    value: identity.name
  }
  {
    name: 'AZURE_CLIENT_ID'
    value: identity.properties.clientId
  }
  {
    name: 'KONECT_AZURE_KEY_ID'
    value: keyUri
  }
  {
    // Download + store open stay inside the 5-minute startup probe; a fresh Entra administrator takes minutes.
    name: 'KONECT_STORE_OPEN_RETRY_SECONDS'
    value: '180'
  }
]
var tokenEnv = [
  {
    name: 'KONECT_INSTALL_TOKEN'
    secretRef: 'install-token'
  }
]

// One revision from the start: env, volume and probes exist in revision 1.
resource app 'Microsoft.App/containerApps@2026-07-01' = {
  name: name
  location: location
  tags: union(tags, {
    'konect-version': checkedVersion
  })
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  properties: {
    environmentId: containerEnv.id
    workloadProfileName: 'Consumption'
    configuration: {
      activeRevisionsMode: 'Single'
      // strict: no token secret and no token variable; the deployment holds no SparkHub credential.
      secrets: bootstrap
        ? [
            {
              name: 'install-token'
              value: installToken
            }
          ]
        : []
      // A private registry is pulled with the managed identity (no registry password anywhere).
      registries: useRegistry
        ? [
            {
              server: registryServer
              identity: identity.id
            }
          ]
        : []
      ingress: {
        external: true
        targetPort: 41800
        transport: 'http'
        allowInsecure: false
        traffic: [
          {
            latestRevision: true
            weight: 100
          }
        ]
      }
    }
    template: {
      containers: [
        {
          name: 'konect'
          image: image
          env: bootstrap ? concat(tokenEnv, konectEnv) : konectEnv
          resources: {
            cpu: json(cpu)
            memory: memory
          }
          volumeMounts: [
            {
              volumeName: 'data'
              mountPath: '/data'
            }
          ]
          // HTTP probes work for the bootstrap and a strict image alike. /v1/health answers 200 while locked.
          probes: [
            {
              type: 'Startup'
              httpGet: health
              initialDelaySeconds: 10
              periodSeconds: 30
              timeoutSeconds: 5
              failureThreshold: 10
            }
            {
              type: 'Liveness'
              httpGet: health
              periodSeconds: 30
              timeoutSeconds: 5
              failureThreshold: 3
            }
          ]
        }
      ]
      scale: {
        minReplicas: 1
        maxReplicas: 1
      }
      // The bootstrap's cache: survives container restarts within a replica.
      volumes: [
        {
          name: 'data'
          storageType: 'EmptyDir'
        }
      ]
    }
  }
  dependsOn: [
    postgresAdmin
    firewall
    registryRole
  ]
}

// ── Outputs (never the token) ────────────────────────────────────────────────

var roleCommand = 'az role assignment create --assignee-object-id ${identity.properties.principalId} --assignee-principal-type ServicePrincipal --role ${cryptoServiceEncryptionUser} --scope ${keyResourceId}'
var registryRoleCommand = 'az role assignment create --assignee-object-id ${identity.properties.principalId} --assignee-principal-type ServicePrincipal --role 7f951dda-4ed3-4680-a7ca-43fe172d538d --scope ${registryId}'
var openStep ='Open ${baseUrl} and click Connect to SparkHub; a Konect manager approves it in SparkHub.'

@description('The instance URL.')
output url string = baseUrl

@description('The Container App\'s generated host (point a custom domain\'s CNAME here).')
output ingressFqdn string = app.properties.configuration.ingress.fqdn

@description('The Key Vault holding the wrapping key.')
output keyVaultName string = keyVaultName

@description('The wrapping key, versionless (KONECT_AZURE_KEY_ID).')
output keyUri string = keyUri

@description('The wrapping key\'s resource ID (existingKeyId of a restore; the scope of the role assignment).')
output keyId string = keyResourceId

@description('The PostgreSQL host.')
output postgresHost string = server.properties.fullyQualifiedDomainName

@description('The PostgreSQL server name (postgresServerName of an in-place restore).')
output postgresServer string = serverName

@description('The managed identity\'s object id.')
output identityPrincipalId string = identity.properties.principalId

@description('What to do next.')
output nextStep string = createRoleAssignments
  ? openStep
  : 'First a platform team grants the identity the key role: ${roleCommand} (Konect stays locked with kms_unavailable until then and heals by itself)${useRegistry ? ', and AcrPull on the registry: ${registryRoleCommand} (the container cannot start until then)' : ''}. Then: ${openStep}'
