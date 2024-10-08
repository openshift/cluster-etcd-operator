// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1

// OpenStackFailureDomainApplyConfiguration represents a declarative configuration of the OpenStackFailureDomain type for use
// with apply.
type OpenStackFailureDomainApplyConfiguration struct {
	AvailabilityZone *string                       `json:"availabilityZone,omitempty"`
	RootVolume       *RootVolumeApplyConfiguration `json:"rootVolume,omitempty"`
}

// OpenStackFailureDomainApplyConfiguration constructs a declarative configuration of the OpenStackFailureDomain type for use with
// apply.
func OpenStackFailureDomain() *OpenStackFailureDomainApplyConfiguration {
	return &OpenStackFailureDomainApplyConfiguration{}
}

// WithAvailabilityZone sets the AvailabilityZone field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the AvailabilityZone field is set to the value of the last call.
func (b *OpenStackFailureDomainApplyConfiguration) WithAvailabilityZone(value string) *OpenStackFailureDomainApplyConfiguration {
	b.AvailabilityZone = &value
	return b
}

// WithRootVolume sets the RootVolume field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the RootVolume field is set to the value of the last call.
func (b *OpenStackFailureDomainApplyConfiguration) WithRootVolume(value *RootVolumeApplyConfiguration) *OpenStackFailureDomainApplyConfiguration {
	b.RootVolume = value
	return b
}
