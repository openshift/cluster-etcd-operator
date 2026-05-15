package tools

// TnfUpdateSetupComponentValue is app.kubernetes.io/component for update-setup snapshot ConfigMaps.
// CEO creates ConfigMaps with this value; the in-cluster update-setup job lists by the same selector.
const TnfUpdateSetupComponentValue = "tnf-update-setup"
