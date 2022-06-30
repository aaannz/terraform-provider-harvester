package virtualmachine

import (
	"context"
	"fmt"
	"strings"

	harvesterutil "github.com/harvester/harvester/pkg/util"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/harvester/terraform-provider-harvester/internal/util"
	"github.com/harvester/terraform-provider-harvester/pkg/client"
	"github.com/harvester/terraform-provider-harvester/pkg/constants"
	"github.com/harvester/terraform-provider-harvester/pkg/helper"
	"github.com/harvester/terraform-provider-harvester/pkg/importer"
)

const (
	vmDeleteTimeout = 300
	vmCreateTimeout = 300
	vmLeasesTimeout = 300
)

func ResourceVirtualMachine() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceVirtualMachineCreate,
		ReadContext:   resourceVirtualMachineRead,
		DeleteContext: resourceVirtualMachineDelete,
		UpdateContext: resourceVirtualMachineUpdate,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Schema: Schema(),
	}
}

func resourceVirtualMachineCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*client.Client)
	namespace := d.Get(constants.FieldCommonNamespace).(string)
	name := d.Get(constants.FieldCommonName).(string)
	toCreate, err := util.ResourceConstruct(d, Creator(c, namespace, name))
	if err != nil {
		return diag.FromErr(err)
	}
	obj, err := c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Create(ctx, toCreate.(*kubevirtv1.VirtualMachine), metav1.CreateOptions{})
	if err != nil {
		return diag.FromErr(err)
	}

	timeoutSeconds := int64(vmCreateTimeout)
	events, err := c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:  fmt.Sprintf("metadata.name=%s", name),
		TimeoutSeconds: &timeoutSeconds,
		Watch:          true,
	})
	if err != nil {
		return diag.FromErr(err)
	}
	isVmReady := false
	for event := range events.ResultChan() {
		if event.Type == watch.Added || event.Type == watch.Modified {
			if event.Object.(*kubevirtv1.VirtualMachine).Status.Ready {
				events.Stop()
				isVmReady = true
			}
		}
	}
	if !isVmReady {
		return diag.Errorf("Timeout waiting for VM %s to be created", name)
	}

	// Gather all interfaces attached to VM for which `wait_for_lease` is set
	waitForLeases := map[string]bool{}
	networkInterfaces := d.Get(constants.FieldVirtualMachineNetworkInterface).([]interface{})
	for _, ni := range networkInterfaces {
		niData := ni.(map[string]interface{})
		if val, ok := niData[constants.FieldNetworkInterfaceWaitForLease].(bool); ok && val {
			waitForLeases[niData[constants.FiledNetworkInterfaceName].(string)] = true
		}
	}

	// For all net interfaces in above gathered list wait until IP address is reported
	if len(waitForLeases) > 0 {
		timeoutSeconds = int64(vmLeasesTimeout)
		events, err = c.HarvesterClient.KubevirtV1().VirtualMachineInstances(namespace).Watch(ctx, metav1.ListOptions{
			FieldSelector:  fmt.Sprintf("metadata.name=%s", name),
			TimeoutSeconds: &timeoutSeconds,
			Watch:          true,
		})
		if err != nil {
			return diag.FromErr(err)
		}
		gotip := false
		for event := range events.ResultChan() {
			if event.Type == watch.Added || event.Type == watch.Modified {
				networks := event.Object.(*kubevirtv1.VirtualMachineInstance).Status.Interfaces
				for _, net := range networks {
					if _, ok := waitForLeases[net.Name]; ok && len(net.IP) > 0 {
						delete(waitForLeases, net.Name)
					}
				}
				if len(waitForLeases) == 0 {
					events.Stop()
					gotip = true
				}
			}
		}
		if !gotip {
			return diag.Errorf("Timeout waiting for VM %s to get IP address", name)
		}
	}

	return resourceVirtualMachineImport(d, obj, nil)
}

func resourceVirtualMachineUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*client.Client)
	namespace, name, err := helper.IDParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	obj, err := c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			d.SetId("")
			return nil
		}
		return diag.FromErr(err)
	}
	toUpdate, err := util.ResourceConstruct(d, Updater(c, obj))
	if err != nil {
		return diag.FromErr(err)
	}
	_, err = c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Update(ctx, toUpdate.(*kubevirtv1.VirtualMachine), metav1.UpdateOptions{})
	if err != nil {
		return diag.FromErr(err)
	}
	return resourceVirtualMachineRead(ctx, d, meta)
}

func resourceVirtualMachineRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*client.Client)
	namespace, name, err := helper.IDParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	vm, err := c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			d.SetId("")
			return nil
		}
		return diag.FromErr(err)
	}
	vmi, err := c.HarvesterClient.KubevirtV1().VirtualMachineInstances(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return diag.FromErr(err)
	}
	return resourceVirtualMachineImport(d, vm, vmi)
}

func resourceVirtualMachineDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*client.Client)
	namespace, name, err := helper.IDParts(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	vm, err := c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			d.SetId("")
			return nil
		}
		return diag.FromErr(err)
	}
	deleteConfigs := make(map[string]bool)
	if diskList, ok := d.GetOk(constants.FieldVirtualMachineDisk); ok {
		for _, disk := range diskList.([]interface{}) {
			r := disk.(map[string]interface{})
			diskName := r[constants.FieldDiskName].(string)
			deleteConfigs[diskName] = r[constants.FieldDiskAutoDelete].(bool)
		}
	}
	removedPVCs := make([]string, 0, len(vm.Spec.Template.Spec.Volumes))
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		if autoDelete, ok := deleteConfigs[volume.Name]; ok && !autoDelete {
			continue
		}
		removedPVCs = append(removedPVCs, volume.PersistentVolumeClaim.ClaimName)
	}
	vmCopy := vm.DeepCopy()
	vmCopy.Annotations[harvesterutil.RemovedPVCsAnnotationKey] = strings.Join(removedPVCs, ",")
	_, err = c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Update(ctx, vmCopy, metav1.UpdateOptions{})
	if err != nil {
		return diag.FromErr(err)
	}
	propagationPolicy := metav1.DeletePropagationForeground
	deleteOptions := metav1.DeleteOptions{PropagationPolicy: &propagationPolicy}
	if err = c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Delete(ctx, name, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return diag.FromErr(err)
	}
	timeoutSeconds := int64(vmDeleteTimeout)
	events, err := c.HarvesterClient.KubevirtV1().VirtualMachines(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:  fmt.Sprintf("metadata.name=%s", name),
		Watch:          true,
		TimeoutSeconds: &timeoutSeconds,
	})
	if err != nil {
		return diag.FromErr(err)
	}
	deleted := false
	for event := range events.ResultChan() {
		if event.Type == watch.Deleted {
			events.Stop()
			deleted = true
		}
	}
	if !deleted {
		return diag.FromErr(fmt.Errorf("timeout waiting for virtualmachine %s to be deleted", d.Id()))
	}
	d.SetId("")
	return nil
}

func resourceVirtualMachineImport(d *schema.ResourceData, vm *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) diag.Diagnostics {
	stateGetter, err := importer.ResourceVirtualMachineStateGetter(vm, vmi)
	if err != nil {
		return diag.FromErr(err)
	}
	return diag.FromErr(util.ResourceStatesSet(d, stateGetter))
}
