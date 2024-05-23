/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	metal "github.com/equinix/equinix-sdk-go/services/metalv1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-packet/api/v1beta1"
	packet "sigs.k8s.io/cluster-api-provider-packet/pkg/cloud/packet"
	"sigs.k8s.io/cluster-api-provider-packet/pkg/cloud/packet/scope"
	clog "sigs.k8s.io/cluster-api/util/log"
)

const (
	force = true
)

var (
	errMissingDevice = errors.New("machine does not exist")
	errFacilityMatch = errors.New("instance facility does not match machine facility")
	errMetroMatch    = errors.New("instance metro does not match machine metro")
)

// PacketMachineReconciler reconciles a PacketMachine object.
type PacketMachineReconciler struct {
	client.Client
	PacketClient *packet.Client

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

type IPAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=packetmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=packetmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/spec;clusters/status;machinesets;machines;machines/status;secret;secret/spec;secret/status,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=kubeadmconfigs;kubeadmconfigs/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;secrets/spec;secrets/status;,verbs=get;list;watch;update;patch

func (r *PacketMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the PacketMachine instance.
	packetmachine := &infrav1.PacketMachine{}
	if err := r.Client.Get(ctx, req.NamespacedName, packetmachine); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "PacketMachine resource not found or already deleted")
			return ctrl.Result{}, nil
		}

		log.Error(err, "Unable to fetch PacketMachine resource")
		return ctrl.Result{}, err
	}

	// AddOwners adds the owners of PacketMachine as k/v pairs to the logger.
	// Specifically, it will add KubeadmControlPlane, MachineSet and MachineDeployment.
	ctx, log, err := clog.AddOwners(ctx, r.Client, packetmachine)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Fetch the Machine.
	machine, err := util.GetOwnerMachine(ctx, r.Client, packetmachine.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to get owner machine")
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Machine Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}

	log = log.WithValues("Machine", klog.KObj(machine))
	ctx = ctrl.LoggerInto(ctx, log)

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterNameLabel))
		return ctrl.Result{}, nil
	}

	log = log.WithValues("Cluster", klog.KObj(cluster))
	ctx = ctrl.LoggerInto(ctx, log)

	// Return early if the object or Cluster is paused.
	if annotations.IsPaused(cluster, packetmachine) {
		log.Info("PacketMachine or linked Cluster is marked as paused. Won't reconcile")
		return ctrl.Result{}, nil
	}

	// Fetch the Packet Cluster
	packetcluster := &infrav1.PacketCluster{}
	packetclusterNamespacedName := client.ObjectKey{
		Namespace: packetmachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, packetclusterNamespacedName, packetcluster); err != nil {
		log.Info("PacketCluster is not available yet")
		return ctrl.Result{}, nil
	}

	// Create the machine scope
	machineScope, err := scope.NewMachineScope(
		scope.MachineScopeParams{
			Client:        r.Client,
			Cluster:       cluster,
			Machine:       machine,
			PacketCluster: packetcluster,
			PacketMachine: packetmachine,
		})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create scope: %w", err)
	}

	// Always close the scope when exiting this function so we can persist any PacketMachine changes.
	defer func() {
		if err := machineScope.Close(ctx); err != nil && rerr == nil {
			log.Error(err, "failed to patch packetmachine")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	// Handle deleted machines
	if !packetmachine.ObjectMeta.DeletionTimestamp.IsZero() {
		err = r.reconcileDelete(ctx, machineScope, packetcluster, cluster)
		return ctrl.Result{}, err
	}
	return r.reconcile(ctx, machineScope, packetcluster, cluster)
}

func (r *PacketMachineReconciler) reconcileSecret(ctx context.Context, packetmachine *infrav1.PacketMachine, packetcluster *infrav1.PacketCluster, cluster *clusterv1.Cluster, deleteflag int) error {
	log := ctrl.LoggerFrom(ctx)
	var oldstring string
	var newstring string

	clusterSecret := &corev1.Secret{}
	clusterSecretNamespacedName := client.ObjectKey{
		Namespace: packetmachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name + "-kubeconfig",
	}
	if err := r.Client.Get(ctx, clusterSecretNamespacedName, clusterSecret); err != nil {
		log.Info("clusterSecret is not available yet")
		return err
	}
	clusterSecretByte, err := json.Marshal(clusterSecret.Data)
	if err != nil {
		return fmt.Errorf("address marshal error occured: %w", err)
	}

	var jsonData map[string]string
	err = json.Unmarshal(clusterSecretByte, &jsonData)
	if err != nil {
		fmt.Printf("Error parsing JSON: %v\n", err)
		return fmt.Errorf("Error parsing JSON error occured: %w", err)
	}

	value := jsonData["value"]
	decodedValue, _ := base64.StdEncoding.DecodeString(string(value))
	decodedString := string(decodedValue)

	lines := strings.Split(decodedString, "\n")

	// Find the index of the line containing the server field
	serverLineIndex := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "    server:") {
			serverLineIndex = i
			break
		}
	}

	// Check if the server line was found
	if serverLineIndex == -1 {
		fmt.Println("Server line not found.")
		return fmt.Errorf("Error Server line not found error occured: %w", err)
	}

	//여러개 삭제되었을때는?? 로직...
	//컨트롤플레인이 하나일때도 좀...
	if deleteflag == 0 {
		oldstring = "https://127.0.0.1:6443"
		newstring = `https://` + string(packetcluster.Status.AvailableServerIPs[0]) + `:6443`
	} else {
		oldstring = `https://` + string(packetcluster.Status.AvailableServerIPs[0]) + `:6443`
		newstring = `https://` + string(packetcluster.Status.AvailableServerIPs[1]) + `:6443`
		fmt.Printf("delete secret %v, %v \n", oldstring, newstring)
	}

	lines[serverLineIndex] = strings.Replace(lines[serverLineIndex], oldstring, newstring, 1)

	// Reconstruct the JSON string
	modifiedJson := strings.Join(lines, "\n")

	jsonDataBytes := []byte(modifiedJson)

	// Encode the byte slice to a Base64 string
	encodedJson := base64.StdEncoding.EncodeToString(jsonDataBytes)

	fmt.Printf("Encoded JSON: %s\n", encodedJson)

	resultMap := map[string][]byte{
		"value": jsonDataBytes,
	}

	clusterSecret.Data = resultMap

	if err := r.Client.Update(ctx, clusterSecret); err != nil {
		fmt.Printf("Error Occured when update clusterSecret %v\n", err)
		return err
	}

	return nil
}

func (r *PacketMachineReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := ctrl.LoggerFrom(ctx)

	clusterToPacketMachines, err := util.ClusterToTypedObjectsMapper(mgr.GetClient(), &infrav1.PacketMachineList{}, mgr.GetScheme())
	if err != nil {
		return fmt.Errorf("failed to create mapper for Cluster to PacketMachines: %w", err)
	}

	err = ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.PacketMachine{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(log, r.WatchFilterValue)).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(infrav1.GroupVersion.WithKind("PacketMachine"))),
		).
		Watches(
			&infrav1.PacketCluster{},
			handler.EnqueueRequestsFromMapFunc(r.PacketClusterToPacketMachines),
		).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterToPacketMachines),
			builder.WithPredicates(
				predicates.ClusterUnpausedAndInfrastructureReady(log),
			),
		).Complete(r)
	if err != nil {
		return fmt.Errorf("failed setting up with a controller manager: %w", err)
	}
	return nil
}

// PacketClusterToPacketMachines is a handler.ToRequestsFunc to be used to enqeue requests for reconciliation
// of PacketMachines.
func (r *PacketMachineReconciler) PacketClusterToPacketMachines(ctx context.Context, o client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)
	result := []ctrl.Request{}
	c, ok := o.(*infrav1.PacketCluster)
	if !ok {
		log.Error(fmt.Errorf("expected a PacketCluster but got a %T", o), "failed to get PacketMachine for PacketCluster") //nolint:goerr113
		return nil
	}

	log = log.WithValues("PacketCluster", c.Name, "Namespace", c.Namespace)

	// Don't handle deleted PacketClusters
	if !c.ObjectMeta.DeletionTimestamp.IsZero() {
		log.V(4).Info("PacketCluster has a deletion timestamp, skipping mapping.")
		return nil
	}

	cluster, err := util.GetOwnerCluster(ctx, r.Client, c.ObjectMeta)
	switch {
	case apierrors.IsNotFound(err) || cluster == nil:
		log.Error(err, "owning cluster is not found, skipping mapping.")
		return result
	case err != nil:
		log.Error(err, "failed to get owning cluster")
		return result
	}

	labels := map[string]string{clusterv1.ClusterNameLabel: cluster.Name}
	machineList := &clusterv1.MachineList{}
	if err := r.Client.List(ctx, machineList, client.InNamespace(c.Namespace), client.MatchingLabels(labels)); err != nil {
		log.Error(err, "failed to get Machines for Cluster")
		return nil
	}
	for _, m := range machineList.Items {
		if m.Spec.InfrastructureRef.Name == "" {
			continue
		}
		name := client.ObjectKey{Namespace: m.Namespace, Name: m.Name}
		result = append(result, ctrl.Request{NamespacedName: name})
	}

	return result
}

func (r *PacketMachineReconciler) reconcile(ctx context.Context, machineScope *scope.MachineScope, packetcluster *infrav1.PacketCluster, cluster *clusterv1.Cluster) (ctrl.Result, error) { //nolint:gocyclo,maintidx
	log := ctrl.LoggerFrom(ctx, "machine", machineScope.Machine.Name, "cluster", machineScope.Cluster.Name)
	log.Info("Reconciling PacketMachine")

	packetmachine := machineScope.PacketMachine
	// If the PacketMachine is in an error state, return early.
	if packetmachine.Status.FailureReason != nil || packetmachine.Status.FailureMessage != nil {
		log.Info("Error state detected, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// If the PacketMachine doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(packetmachine, infrav1.MachineFinalizer)
	if err := machineScope.PatchObject(ctx); err != nil {
		log.Error(err, "unable to patch object")
	}

	if !machineScope.Cluster.Status.InfrastructureReady {
		log.Info("Cluster infrastructure is not ready yet")
		conditions.MarkFalse(machineScope.PacketMachine, infrav1.DeviceReadyCondition, infrav1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	// Make sure bootstrap data secret is available and populated.
	if machineScope.Machine.Spec.Bootstrap.DataSecretName == nil {
		log.Info("Bootstrap data secret is not yet available")
		conditions.MarkFalse(machineScope.PacketMachine, infrav1.DeviceReadyCondition, infrav1.WaitingForBootstrapDataReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	deviceID := machineScope.GetDeviceID()
	var (
		dev                  *metal.Device
		addrs                []corev1.NodeAddress
		err                  error
		controlPlaneEndpoint *metal.IPReservation
		resp                 *http.Response
	)

	if deviceID != "" {
		// If we already have a device ID, then retrieve the device using the
		// device ID. This means that the Machine has already been created
		// and we successfully recorded the device ID.
		dev, resp, err = r.PacketClient.GetDevice(ctx, deviceID) //nolint:bodyclose // see https://github.com/timakin/bodyclose/issues/42
		if err != nil {
			if resp != nil {
				if resp.StatusCode == http.StatusNotFound {
					machineScope.SetFailureReason(capierrors.UpdateMachineError)
					machineScope.SetFailureMessage(fmt.Errorf("failed to find device: %w", err))
					log.Error(err, "unable to find device")
					conditions.MarkFalse(machineScope.PacketMachine, infrav1.DeviceReadyCondition, infrav1.InstanceNotFoundReason, clusterv1.ConditionSeverityError, err.Error())
				} else if resp.StatusCode == http.StatusForbidden {
					machineScope.SetFailureReason(capierrors.UpdateMachineError)
					log.Error(err, "device failed to provision")
					machineScope.SetFailureMessage(fmt.Errorf("device failed to provision: %w", err))
					conditions.MarkFalse(machineScope.PacketMachine, infrav1.DeviceReadyCondition, infrav1.InstanceProvisionFailedReason, clusterv1.ConditionSeverityError, err.Error())
				}
			}

			return ctrl.Result{}, err
		}
	}

	if dev == nil {
		// We don't yet have a device ID, check to see if we've already
		// created a device by using the tags that we assign to devices
		// on creation.
		dev, err = r.PacketClient.GetDeviceByTags(
			ctx,
			machineScope.PacketCluster.Spec.ProjectID,
			packet.DefaultCreateTags(machineScope.Namespace(), machineScope.Machine.Name, machineScope.Cluster.Name),
		)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if dev == nil {
		// We weren't able to find a device by either device ID or by tags,
		// so we need to create a new device.

		// Avoid a flickering condition between InstanceProvisionStarted and InstanceProvisionFailed if there's a persistent failure with createInstance
		if conditions.GetReason(machineScope.PacketMachine, infrav1.DeviceReadyCondition) != infrav1.InstanceProvisionFailedReason {
			conditions.MarkFalse(machineScope.PacketMachine, infrav1.DeviceReadyCondition, infrav1.InstanceProvisionStartedReason, clusterv1.ConditionSeverityInfo, "")
			if patchErr := machineScope.PatchObject(ctx); patchErr != nil {
				log.Error(patchErr, "failed to patch conditions")
				return ctrl.Result{}, patchErr
			}
		}

		createDeviceReq := packet.CreateDeviceRequest{
			MachineScope: machineScope,
			ExtraTags:    packet.DefaultCreateTags(machineScope.Namespace(), machineScope.Machine.Name, machineScope.Cluster.Name),
		}

		// when a node is a control plane node we need the elastic IP
		// to template out the kube-vip deployment
		if machineScope.IsControlPlane() {
			controlPlaneEndpoint, _ = r.PacketClient.GetIPByClusterIdentifier(
				ctx,
				machineScope.Cluster.Namespace,
				machineScope.Cluster.Name,
				machineScope.PacketCluster.Spec.ProjectID)
			if machineScope.PacketCluster.Spec.VIPManager == "CPEM" {
				if len(controlPlaneEndpoint.Assignments) == 0 {
					a := corev1.NodeAddress{
						Type:    corev1.NodeExternalIP,
						Address: controlPlaneEndpoint.GetAddress(),
					}
					addrs = append(addrs, a)
				}
			}
			createDeviceReq.ControlPlaneEndpoint = controlPlaneEndpoint.GetAddress()
		}

		dev, err = r.PacketClient.NewDevice(ctx, createDeviceReq)

		switch {
		// TODO: find a better way than parsing the error messages for this.
		case err != nil && strings.Contains(err.Error(), " no available hardware reservations "):
			// Do not treat an error indicating there are no hardware reservations available as fatal
			return ctrl.Result{}, fmt.Errorf("failed to create machine %s: %w", machineScope.Name(), err)
		case err != nil && strings.Contains(err.Error(), "Server is not provisionable"):
			// Do not treat an error indicating that reserved hardware is not provisionable as fatal
			// This occurs when reserved hardware is in the process of being deprovisioned
			return ctrl.Result{}, fmt.Errorf("failed to create machine %s: %w", machineScope.Name(), err)
		case err != nil && strings.Contains(err.Error(), " unexpected EOF"):
			// Do not treat unexpected EOF as fatal, provisioning likely is proceeding
		case err != nil:
			errs := fmt.Errorf("failed to create machine %s: %w", machineScope.Name(), err)
			machineScope.SetFailureReason(capierrors.CreateMachineError)
			machineScope.SetFailureMessage(errs)
			conditions.MarkFalse(machineScope.PacketMachine, infrav1.DeviceReadyCondition, infrav1.InstanceProvisionFailedReason, clusterv1.ConditionSeverityError, err.Error())

			return ctrl.Result{}, errs
		}
	}

	// we do not need to set this as equinixmetal://<id> because SetProviderID() does the formatting for us
	machineScope.SetProviderID(dev.GetId())
	machineScope.SetInstanceStatus(infrav1.PacketResourceStatus(dev.GetState()))

	if machineScope.PacketCluster.Spec.VIPManager == "KUBE_VIP" {
		if err := r.PacketClient.EnsureNodeBGPEnabled(ctx, dev.GetId()); err != nil {
			// Do not treat an error enabling bgp on machine as fatal
			return ctrl.Result{RequeueAfter: time.Second * 20}, fmt.Errorf("failed to enable bgp on machine %s: %w", machineScope.Name(), err)
		}
	}

	deviceAddr := r.PacketClient.GetDeviceAddresses(dev)
	// testtesttest
	if machineScope.IsControlPlane() {
		var IpAddresses []IPAddress
		// var availableServerIP string

		//testtesttest - join되는 애들 packetcluster의 apiendpoint를 바꾸려고 함. - 이건 되는데, 문제는 처음 생성.
		// if len(packetcluster.Status.AvailableServerIPs) != 0 {
		// 	packetcluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
		// 		Host: packetcluster.Status.AvailableServerIPs[0],
		// 		Port: 6443,
		// 	}
		// 	if err := r.Client.Update(ctx, packetcluster); err != nil {
		// 		fmt.Printf("Error Occured when update spec of PacketCluster  %v", err)
		// 		return ctrl.Result{}, err
		// 	}
		// }

		//testtesttest

		adrbyte, err := json.Marshal(deviceAddr)
		fmt.Printf("ADDR BYTE %v", string(adrbyte))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("address marshal error occured: %w", err)
		}
		if len(adrbyte) != 0 {
			if err := json.Unmarshal(adrbyte, &IpAddresses); err != nil {
				return ctrl.Result{}, fmt.Errorf("address unmarshal error occured: %w", err)
			}

			//외부 IP일 경우에만 사용
			availableServerIPs := make([]string, 0)

			//외부 IP일 경우에만 사용
			for _, ip := range IpAddresses {
				// 실제로는 이렇게 내부 IP를 바라보게 하는 코드가 맞음.
				// if ip.Type == "InternalIP" {
				// 	fmt.Printf("loook at the IP %v, %T", ip.Address, ip.Address)
				// 	availableServerIP = ip.Address
				// 	if !slices.Contains(packetcluster.Status.AvailableServerIPs, availableServerIP) {
				// 		packetcluster.Status.AvailableServerIPs = append(packetcluster.Status.AvailableServerIPs, availableServerIP)
				// 	}
				// }

				// 테스트를 위해서 외부 IP를 가져오는 것으로 테스트
				if ip.Type == "ExternalIP" {
					// fmt.Printf("loook at the IP %v, %T\n", ip.Address, ip.Address)
					availableServerIPs = append(availableServerIPs, ip.Address)
					// 	fmt.Fprintf(os.Stdout, `available server ip list %v
					// `, []any{availableServerIPs}...)
					if !slices.Contains(packetcluster.Status.AvailableServerIPs, availableServerIPs[0]) {
						packetcluster.Status.AvailableServerIPs = append(packetcluster.Status.AvailableServerIPs, availableServerIPs[0])
					}
				}
			}
		}
	}
	if err := r.Client.Status().Update(ctx, packetcluster); err != nil {
		fmt.Printf("Error Occured when update AvailableServerIp of packetCluster %v", err)
		return ctrl.Result{}, err
	}

	if len(packetcluster.Status.AvailableServerIPs) != 0 {
		cluster.Spec.ControlPlaneEndpoint.Host = packetcluster.Status.AvailableServerIPs[0]

		//testtesttest
		//make change secret function
		deleteflag := 0
		err := r.reconcileSecret(ctx, packetmachine, packetcluster, cluster, deleteflag)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.Client.Update(ctx, packetcluster); err != nil {
		fmt.Printf("Error Occured when update Spec Controlplane of packetCluster %v", err)
		return ctrl.Result{}, err
	}

	if err := r.Client.Update(ctx, cluster); err != nil {
		fmt.Printf("Error Occured when update Spec Controlplane of packetCluster %v", err)
		return ctrl.Result{}, err
	}

	machineScope.SetAddresses(append(addrs, deviceAddr...))

	// Proceed to reconcile the PacketMachine state.
	var result reconcile.Result

	switch infrav1.PacketResourceStatus(dev.GetState()) {
	case infrav1.PacketResourceStatusNew, infrav1.PacketResourceStatusQueued, infrav1.PacketResourceStatusProvisioning:
		log.Info("Machine instance is pending", "instance-id", machineScope.ProviderID())
		machineScope.SetNotReady()
		result = ctrl.Result{RequeueAfter: 10 * time.Second}
	case infrav1.PacketResourceStatusRunning:
		log.Info("Machine instance is active", "instance-id", machineScope.ProviderID())

		if machineScope.PacketCluster.Spec.VIPManager == "CPEM" {
			controlPlaneEndpoint, _ = r.PacketClient.GetIPByClusterIdentifier(
				ctx,
				machineScope.Cluster.Namespace,
				machineScope.Cluster.Name,
				machineScope.PacketCluster.Spec.ProjectID)
			if len(controlPlaneEndpoint.Assignments) == 0 && machineScope.IsControlPlane() {
				apiRequest := r.PacketClient.DevicesApi.CreateIPAssignment(ctx, *dev.Id).IPAssignmentInput(metal.IPAssignmentInput{
					Address: controlPlaneEndpoint.GetAddress(),
				})
				if _, _, err := apiRequest.Execute(); err != nil { //nolint:bodyclose // see https://github.com/timakin/bodyclose/issues/42
					log.Error(err, "err assigining elastic ip to control plane. retrying...")
					return ctrl.Result{RequeueAfter: time.Second * 20}, nil
				}
			}
		}

		machineScope.SetReady()
		conditions.MarkTrue(machineScope.PacketMachine, infrav1.DeviceReadyCondition)

		result = ctrl.Result{}
	default:
		machineScope.SetNotReady()
		log.Info("Equinix Metal device state is undefined", "state", dev.GetState(), "device-id", machineScope.ProviderID())
		machineScope.SetFailureReason(capierrors.UpdateMachineError)
		machineScope.SetFailureMessage(fmt.Errorf("instance status %q is unexpected", dev.GetState())) //nolint:goerr113
		conditions.MarkUnknown(machineScope.PacketMachine, infrav1.DeviceReadyCondition, "", "")

		result = ctrl.Result{}
	}

	// If Metro or Facility has changed in the spec, verify that the facility's metro is compatible with the requested spec change.
	deviceFacility := dev.Facility.Code
	deviceMetro := dev.Metro.Code

	if machineScope.PacketMachine.Spec.Facility != "" && machineScope.PacketMachine.Spec.Facility != *deviceFacility {
		return ctrl.Result{}, fmt.Errorf("%w: %s != %s", errFacilityMatch, machineScope.PacketMachine.Spec.Facility, *deviceFacility)
	}

	if machineScope.PacketMachine.Spec.Metro != "" && machineScope.PacketMachine.Spec.Metro != *deviceMetro {
		return ctrl.Result{}, fmt.Errorf("%w: %s != %s", errMetroMatch, machineScope.PacketMachine.Spec.Facility, *deviceMetro)
	}

	return result, nil
}

func (r *PacketMachineReconciler) reconcileDelete(ctx context.Context, machineScope *scope.MachineScope, packetcluster *infrav1.PacketCluster, cluster *clusterv1.Cluster) error {
	log := ctrl.LoggerFrom(ctx, "machine", machineScope.Machine.Name, "cluster", machineScope.Cluster.Name)
	log.Info("Reconciling Delete PacketMachine")

	packetmachine := machineScope.PacketMachine
	deviceID := machineScope.GetDeviceID()

	var device *metal.Device

	if deviceID == "" {
		// If no device ID was recorded, check to see if there are any instances
		// that match by tags
		dev, err := r.PacketClient.GetDeviceByTags(
			ctx,
			machineScope.PacketCluster.Spec.ProjectID,
			packet.DefaultCreateTags(machineScope.Namespace(), machineScope.Machine.Name, machineScope.Cluster.Name),
		)
		if err != nil {
			return err
		}

		if dev == nil {
			log.Info("Server not found by tags, nothing left to do")
			controllerutil.RemoveFinalizer(packetmachine, infrav1.MachineFinalizer)
			return nil
		}

		device = dev
	} else {
		var resp *http.Response
		// Otherwise, try to retrieve the device by the providerID
		dev, resp, err := r.PacketClient.GetDevice(ctx, deviceID) //nolint:bodyclose // see https://github.com/timakin/bodyclose/issues/42
		if err != nil {
			if resp != nil {
				if resp.StatusCode == http.StatusNotFound {
					// When the server does not exist we do not have anything left to do.
					// Probably somebody manually deleted the server from the UI or via API.
					log.Info("Server not found by id, nothing left to do")
					controllerutil.RemoveFinalizer(packetmachine, infrav1.MachineFinalizer)
					return nil
				}

				if resp.StatusCode == http.StatusForbidden {
					// When a server fails to provision it will return a 403
					log.Info("Server appears to have failed provisioning, nothing left to do")
					controllerutil.RemoveFinalizer(packetmachine, infrav1.MachineFinalizer)
					return nil
				}
			}

			return fmt.Errorf("error retrieving machine status %s: %w", packetmachine.Name, err)
		}

		device = dev
	}

	// We should never get there but this is a safetly check
	if device == nil {
		controllerutil.RemoveFinalizer(packetmachine, infrav1.MachineFinalizer)
		return fmt.Errorf("%w: %s", errMissingDevice, packetmachine.Name)
	}

	//testtesttest
	deviceAddr := r.PacketClient.GetDeviceAddresses(device)
	if machineScope.IsControlPlane() {
		//외부 IP일 경우
		var IpAddresses []IPAddress
		var availableServerIP string

		adrbyte, err := json.Marshal(deviceAddr)
		fmt.Printf("ADDR BYTE %v", string(adrbyte))
		if err != nil {
			return fmt.Errorf("address marshal error occured: %w", err)
		}
		if len(adrbyte) != 0 {
			if err := json.Unmarshal(adrbyte, &IpAddresses); err != nil {
				return fmt.Errorf("address unmarshal error occured: %w", err)
			}
			//외부 IP일 경우에만 사용
			availableServerIPs := make([]string, 0)
			for _, ip := range IpAddresses {
				// 실제로는 이렇게 내부 IP를 바라보게 하는 코드가 맞음.
				if ip.Type == "InternalIP" {
					fmt.Printf("loook at the IP %v, %T", ip.Address, ip.Address)
					availableServerIP = ip.Address
					fmt.Printf("INTERNALIP %v", availableServerIP)
				}

				// 테스트를 위해서 외부 IP를 가져오는 것으로 테스트
				if ip.Type == "ExternalIP" {
					fmt.Printf("loook at the external IP %v, %T\n", ip.Address, ip.Address)
					availableServerIPs = append(availableServerIPs, ip.Address)
				}
			}

			//packetcluster, cluster, kubeconfig secret 변경
			//이것도 컨트롤플레인 한개일 경우랑 한번에 여러개 지워질 경우를 대비해야함. 지금은 약간 임시방편
			//관련없지만 rancher에서 cattle-system 연동안되는것도 확인
			if availableServerIPs[0] == packetcluster.Status.AvailableServerIPs[0] {
				deleteflag := 1
				err := r.reconcileSecret(ctx, packetmachine, packetcluster, cluster, deleteflag)
				if err != nil {
					return err
				}
				cluster.Spec.ControlPlaneEndpoint.Host = packetcluster.Status.AvailableServerIPs[1]
			}

			if slices.Contains(packetcluster.Status.AvailableServerIPs, availableServerIPs[0]) {
				index := slices.Index(packetcluster.Status.AvailableServerIPs, availableServerIPs[0])
				packetcluster.Status.AvailableServerIPs = slices.Delete(packetcluster.Status.AvailableServerIPs, index, index+1)
			}
		}
	}
	if err := r.Client.Update(ctx, cluster); err != nil {
		fmt.Printf("Error Occured when update Cluster %v", err)
		return err
	}

	if err := r.Client.Status().Update(ctx, packetcluster); err != nil {
		fmt.Printf("Error Occured when update Cluster %v", err)
		return err
	}

	packetcluster = &infrav1.PacketCluster{}
	packetclusterNamespacedName := client.ObjectKey{
		Namespace: packetmachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, packetclusterNamespacedName, packetcluster); err != nil {
		log.Info("PacketCluster is not available yet")
		return err
	}

	if err := r.Client.Update(ctx, packetcluster); err != nil {
		fmt.Printf("Error Occured when after delete update AvailableServerIp of packetCluster %v", err)
		return err
	}

	apiRequest := r.PacketClient.DevicesApi.DeleteDevice(ctx, device.GetId()).ForceDelete(force)
	if _, err := apiRequest.Execute(); err != nil { //nolint:bodyclose // see https://github.com/timakin/bodyclose/issues/42
		return fmt.Errorf("failed to delete the machine: %w", err)
	}

	controllerutil.RemoveFinalizer(packetmachine, infrav1.MachineFinalizer)
	return nil
}
