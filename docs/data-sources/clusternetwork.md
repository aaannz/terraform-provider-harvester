---
# generated by https://github.com/hashicorp/terraform-plugin-docs
page_title: "harvester_clusternetwork Data Source - terraform-provider-harvester"
subcategory: ""
description: |-
  
---

# harvester_clusternetwork (Data Source)



## Example Usage

```terraform
data "harvester_clusternetwork" "vlan" {
  name = "vlan"
}
```

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- **name** (String) A unique name

### Optional

- **id** (String) The ID of this resource.

### Read-Only

- **default_physical_nic** (String)
- **description** (String) Any text you want that better describes this resource
- **enable** (Boolean)
- **state** (String)
- **tags** (Map of String)


