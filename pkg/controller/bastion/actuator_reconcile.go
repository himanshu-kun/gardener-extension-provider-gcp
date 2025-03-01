// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bastion

import (
	"context"
	"fmt"
	"reflect"
	"time"

	gcpclient "github.com/gardener/gardener-extension-provider-gcp/pkg/internal/client"

	"github.com/gardener/gardener/extensions/pkg/controller"
	ctrlerror "github.com/gardener/gardener/extensions/pkg/controller/error"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bastionEndpoints collects the endpoints the bastion host provides; the
// private endpoint is important for opening a port on the worker node
// ingress firewall rule to allow SSH from that node, the public endpoint is where
// the end user connects to establish the SSH connection.
type bastionEndpoints struct {
	private *corev1.LoadBalancerIngress
	public  *corev1.LoadBalancerIngress
}

// Ready returns true if both public and private interfaces each have either
// an IP or a hostname or both.
func (be *bastionEndpoints) Ready() bool {
	return be != nil && IngressReady(be.private) && IngressReady(be.public)
}

func (a *actuator) Reconcile(ctx context.Context, bastion *extensionsv1alpha1.Bastion, cluster *controller.Cluster) error {
	logger := a.logger.WithValues("bastion", client.ObjectKeyFromObject(bastion), "operation", "reconcile")

	serviceAccount, err := getServiceAccount(ctx, a, bastion)
	if err != nil {
		return fmt.Errorf("failed to get service account: %w", err)
	}

	gcpClient, err := createGCPClient(ctx, serviceAccount)
	if err != nil {
		return fmt.Errorf("failed to create GCP client: %w", err)
	}

	opt, err := DetermineOptions(bastion, cluster, serviceAccount.ProjectID)
	if err != nil {
		return fmt.Errorf("failed to determine Options: %w", err)
	}

	if opt.Zone == "" {
		opt.Zone, err = getDefaultGCPZone(ctx, gcpClient, opt, cluster.Shoot.Spec.Region)
		if err != nil {
			return err
		}
	}

	err = controller.TryUpdateStatus(ctx, retry.DefaultBackoff, a.Client(), bastion, func() error {
		bytes, err := marshalProviderStatus(opt.Zone)
		if err != nil {
			return err
		}
		bastion.Status.ProviderStatus = &runtime.RawExtension{Raw: bytes}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to store status.providerStatus for zone: %s", opt.Zone)
	}

	err = ensureFirewallRules(ctx, gcpClient, opt)
	if err != nil {
		return fmt.Errorf("failed to ensure firewall rule: %w", err)
	}

	err = ensureDisk(ctx, gcpClient, opt)
	if err != nil {
		return err
	}

	instance, err := ensureComputeInstance(ctx, logger, bastion, gcpClient, opt)
	if err != nil {
		return err
	}

	// check if the instance already exists and has an IP
	endpoints, err := getInstanceEndpoints(instance)
	if err != nil {
		return err
	}

	if !endpoints.Ready() {
		return &ctrlerror.RequeueAfterError{
			// requeue rather soon, so that the user (most likely gardenctl eventually)
			// doesn't have to wait too long for the public endpoint to become available
			RequeueAfter: 5 * time.Second,
			Cause:        fmt.Errorf("bastion instance has no public/private endpoints yet"),
		}
	}

	// once a public endpoint is available, publish the endpoint on the
	// Bastion resource to notify upstream about the ready instance
	return controller.TryUpdateStatus(ctx, retry.DefaultBackoff, a.Client(), bastion, func() error {
		bastion.Status.Ingress = *endpoints.public
		return nil
	})

}

func ensureFirewallRules(ctx context.Context, gcpclient gcpclient.Interface, opt *Options) error {
	firewallList := []*compute.Firewall{IngressAllowSSH(opt), EgressDenyAll(opt), EgressAllowOnly(opt)}

	for _, item := range firewallList {
		if err := createFirewallRuleIfNotExist(ctx, gcpclient, opt, item); err != nil {
			return err
		}
	}

	firewall, err := getFirewallRule(ctx, gcpclient, opt, IngressAllowSSH(opt).Name)
	if err != nil || firewall == nil {
		return fmt.Errorf("could not get firewall rule: %w", err)
	}

	currentCIDRs := firewall.SourceRanges
	wantedCIDRs := opt.CIDRs

	if !reflect.DeepEqual(currentCIDRs, wantedCIDRs) {
		return patchFirewallRule(ctx, gcpclient, opt, IngressAllowSSH(opt).Name)
	}

	return nil
}

func ensureComputeInstance(ctx context.Context, logger logr.Logger, bastion *extensionsv1alpha1.Bastion, gcpclient gcpclient.Interface, opt *Options) (*compute.Instance, error) {
	instance, err := getBastionInstance(ctx, gcpclient, opt)
	if instance != nil || err != nil {
		return instance, err
	}

	logger.Info("Creating new bastion compute instance")
	computeInstance := computeInstanceDefine(opt, bastion.Spec.UserData)
	_, err = gcpclient.Instances().Insert(opt.ProjectID, opt.Zone, computeInstance).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to create bastion compute instance: %w", err)
	}

	instance, err = getBastionInstance(ctx, gcpclient, opt)
	if instance != nil || err != nil {
		return instance, err
	}

	return nil, fmt.Errorf("failed to get (create) bastion compute instance: %w", err)
}

func getInstanceEndpoints(instance *compute.Instance) (*bastionEndpoints, error) {
	if instance == nil {
		return nil, fmt.Errorf("compute instance can't be nil")
	}

	if instance.Status != "RUNNING" {
		return nil, fmt.Errorf("instance not running, status: %s", instance.Status)
	}

	endpoints := &bastionEndpoints{}

	networkInterfaces := instance.NetworkInterfaces

	if len(networkInterfaces) == 0 {
		return nil, fmt.Errorf("no network interfaces found: %s", instance.Name)
	}

	internalIP := &networkInterfaces[0].NetworkIP

	if len(networkInterfaces[0].AccessConfigs) == 0 {
		return nil, fmt.Errorf("no access config found for network interface: %s", instance.Name)
	}

	externalIP := &networkInterfaces[0].AccessConfigs[0].NatIP

	if ingress := addressToIngress(&instance.Name, internalIP); ingress != nil {
		endpoints.private = ingress
	}

	// GCP does not automatically assign a public dns name to the instance (in contrast to e.g. AWS).
	// As we provide an externalIP to connect to the bastion, having a public dns name would just be an alternative way to connect to the bastion.
	// Out of this reason, we spare the effort to create a PTR record (see https://cloud.google.com/compute/docs/instances/create-ptr-record#api) just for the sake of having it.
	if ingress := addressToIngress(nil, externalIP); ingress != nil {
		endpoints.public = ingress
	}

	return endpoints, nil
}

// IngressReady returns true if either an IP or a hostname or both are set.
func IngressReady(ingress *corev1.LoadBalancerIngress) bool {
	return ingress != nil && (ingress.Hostname != "" || ingress.IP != "")
}

// addressToIngress converts the IP address into a
// corev1.LoadBalancerIngress resource. If both arguments are nil, then
// nil is returned.
func addressToIngress(dnsName *string, ipAddress *string) *corev1.LoadBalancerIngress {
	var ingress *corev1.LoadBalancerIngress

	if ipAddress != nil || dnsName != nil {
		ingress = &corev1.LoadBalancerIngress{}
		if dnsName != nil {
			ingress.Hostname = *dnsName
		}

		if ipAddress != nil {
			ingress.IP = *ipAddress
		}
	}

	return ingress
}

func ensureDisk(ctx context.Context, gcpclient gcpclient.Interface, opt *Options) error {
	disk, err := getDisk(ctx, gcpclient, opt)
	if disk != nil || err != nil {
		return err
	}

	logger.Info("create new bastion compute instance disk")
	disk = diskDefine(opt.Zone, opt.DiskName)
	_, err = gcpclient.Disks().Insert(opt.ProjectID, opt.Zone, disk).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to create compute instance disk: %w", err)
	}

	disk, err = getDisk(ctx, gcpclient, opt)
	if disk != nil || err != nil {
		return err
	}

	return fmt.Errorf("failed to get (create) compute instance disk: %w", err)
}

func computeInstanceDefine(opt *Options, userData []byte) *compute.Instance {
	return &compute.Instance{
		Disks:              disksDefine(opt),
		DeletionProtection: false,
		Description:        "Bastion Instance",
		Name:               opt.BastionInstanceName,
		Zone:               opt.Zone,
		MachineType:        machineTypeDefine(opt),
		NetworkInterfaces:  networkInterfacesDefine(opt),
		Tags:               &compute.Tags{Items: []string{opt.BastionInstanceName}},
		Metadata:           &compute.Metadata{Items: metadataItemsDefine(userData)},
	}
}

func metadataItemsDefine(userData []byte) []*compute.MetadataItems {
	return []*compute.MetadataItems{
		{
			Key:   "startup-script",
			Value: pointer.StringPtr(string(userData)),
		},
		{
			Key:   "block-project-ssh-keys",
			Value: pointer.StringPtr("TRUE"),
		},
	}
}

func machineTypeDefine(opt *Options) string {
	return fmt.Sprintf("zones/%s/machineTypes/n1-standard-1", opt.Zone)
}

func networkInterfacesDefine(opt *Options) []*compute.NetworkInterface {
	return []*compute.NetworkInterface{
		{
			Network:       opt.Network,
			Subnetwork:    opt.Subnetwork,
			AccessConfigs: []*compute.AccessConfig{{Name: "External NAT", Type: "ONE_TO_ONE_NAT"}},
		},
	}
}

func disksDefine(opt *Options) []*compute.AttachedDisk {
	return []*compute.AttachedDisk{
		{
			AutoDelete: true,
			Boot:       true,
			DiskSizeGb: 10,
			Source:     fmt.Sprintf("projects/%s/zones/%s/disks/%s", opt.ProjectID, opt.Zone, opt.DiskName),
			Mode:       "READ_WRITE",
		},
	}
}

func diskDefine(zone string, diskName string) *compute.Disk {
	return &compute.Disk{
		Description: "Gardenctl Bastion disk",
		Name:        diskName,
		SizeGb:      10,
		SourceImage: "projects/debian-cloud/global/images/family/debian-10",
		Zone:        zone,
	}
}
