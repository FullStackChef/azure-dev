targetScope = 'subscription'

@minLength(1)
@maxLength(64)
@description('Name of the the environment which is used to generate a short unqiue hash used in all resources.')
param name string

@description('Primary location for all resources')
param location string

@description('A time to mark on created resource groups, so they can be cleaned up via an automated process.')
param deleteAfterTime string = dateTimeAdd(utcNow('o'), 'PT1H')

resource resourceGroup 'Microsoft.Resources/resourceGroups@2021-04-01' = {
  name: '${name}-rg'
  location: location
  tags: {
    DeleteAfter: deleteAfterTime
  }
}

module resources './resources.bicep' = {
  name: '${resourceGroup.name}res'
  scope: resourceGroup
  params: {
    name: toLower(name)
    location: location
  }
}

output AZURE_STORAGE_ACCOUNT_ID string = resources.outputs.AZURE_STORAGE_ACCOUNT_ID
output AZURE_STORAGE_ACCOUNT_NAME string = resources.outputs.AZURE_STORAGE_ACCOUNT_NAME