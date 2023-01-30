// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1

import (
	v1 "github.com/openshift/api/config/v1"
)

// FailureDomainsApplyConfiguration represents an declarative configuration of the FailureDomains type for use
// with apply.
type FailureDomainsApplyConfiguration struct {
	Platform *v1.PlatformType                        `json:"platform,omitempty"`
	AWS      *[]AWSFailureDomainApplyConfiguration   `json:"aws,omitempty"`
	Azure    *[]AzureFailureDomainApplyConfiguration `json:"azure,omitempty"`
	GCP      *[]GCPFailureDomainApplyConfiguration   `json:"gcp,omitempty"`
}

// FailureDomainsApplyConfiguration constructs an declarative configuration of the FailureDomains type for use with
// apply.
func FailureDomains() *FailureDomainsApplyConfiguration {
	return &FailureDomainsApplyConfiguration{}
}

// WithPlatform sets the Platform field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Platform field is set to the value of the last call.
func (b *FailureDomainsApplyConfiguration) WithPlatform(value v1.PlatformType) *FailureDomainsApplyConfiguration {
	b.Platform = &value
	return b
}

func (b *FailureDomainsApplyConfiguration) ensureAWSFailureDomainApplyConfigurationExists() {
	if b.AWS == nil {
		b.AWS = &[]AWSFailureDomainApplyConfiguration{}
	}
}

// WithAWS adds the given value to the AWS field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, values provided by each call will be appended to the AWS field.
func (b *FailureDomainsApplyConfiguration) WithAWS(values ...*AWSFailureDomainApplyConfiguration) *FailureDomainsApplyConfiguration {
	b.ensureAWSFailureDomainApplyConfigurationExists()
	for i := range values {
		if values[i] == nil {
			panic("nil value passed to WithAWS")
		}
		*b.AWS = append(*b.AWS, *values[i])
	}
	return b
}

func (b *FailureDomainsApplyConfiguration) ensureAzureFailureDomainApplyConfigurationExists() {
	if b.Azure == nil {
		b.Azure = &[]AzureFailureDomainApplyConfiguration{}
	}
}

// WithAzure adds the given value to the Azure field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, values provided by each call will be appended to the Azure field.
func (b *FailureDomainsApplyConfiguration) WithAzure(values ...*AzureFailureDomainApplyConfiguration) *FailureDomainsApplyConfiguration {
	b.ensureAzureFailureDomainApplyConfigurationExists()
	for i := range values {
		if values[i] == nil {
			panic("nil value passed to WithAzure")
		}
		*b.Azure = append(*b.Azure, *values[i])
	}
	return b
}

func (b *FailureDomainsApplyConfiguration) ensureGCPFailureDomainApplyConfigurationExists() {
	if b.GCP == nil {
		b.GCP = &[]GCPFailureDomainApplyConfiguration{}
	}
}

// WithGCP adds the given value to the GCP field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, values provided by each call will be appended to the GCP field.
func (b *FailureDomainsApplyConfiguration) WithGCP(values ...*GCPFailureDomainApplyConfiguration) *FailureDomainsApplyConfiguration {
	b.ensureGCPFailureDomainApplyConfigurationExists()
	for i := range values {
		if values[i] == nil {
			panic("nil value passed to WithGCP")
		}
		*b.GCP = append(*b.GCP, *values[i])
	}
	return b
}
