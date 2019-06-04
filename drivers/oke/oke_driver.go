package oke

/*
* This is the main driver for the Rancher plug-in to provide CRUD operations for Oracle Container Engine.
*  The driver implements interface required by Rancher (Create, Update, Remove, Get*, etc).
 */

// TODO add some integration tests
// TODO support private subnets

import (
	"encoding/json"
	"fmt"
	"github.com/oracle/oci-go-sdk/common"
	"github.com/pkg/errors"
	"github.com/rancher/kontainer-engine/drivers/options"
	"github.com/rancher/kontainer-engine/types"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"io/ioutil"
)

const (
	defaultNumNodes            = 0
	defaultNumNodeSubnets      = 3
	defaultVCNNamePrefix       = "oke-"
	defaultServiceSubnetPrefix = "oke-"
)

type Driver struct {
	driverCapabilities types.Capabilities
}

type state struct {
	// Should the Kubernetes dashboard be enabled
	EnableKubernetesDashboard bool

	// Should the Helm server (Tiller) be enabled
	EnableTiller bool

	// TODO add support for worker nodes in private subnets
	// TODO currently unused
	PrivateNodes bool

	// The name of the cluster (and default node pool)
	Name string

	// The Oracle Cloud ID (OCID) of the tenancy
	TenancyID string

	// The OCID of the compartment
	CompartmentID string

	// The user OCID
	UserOCID string

	// The path to the private API Key that is associated with the user and has access the tenancy/compartment
	PrivateKeyPath string
	// The contents the private API Key that is associated with the user and has access the tenancy/compartment
	PrivateKeyContents string

	// The API Key Fingerprint
	Fingerprint string

	// The region where the cluster will be hosted
	Region string

	// The passphrase for the private key
	// TODO currently unused
	PrivateKeyPassphrase string

	// The description of the cluster
	// TODO currently unused
	Description string

	// Should cluster creation operation wait until nodes are active
	// TODO currently unused
	WaitNodesActive int64

	// The labels specified during the Kubernetes creation
	// TODO currently unused
	KubernetesLabels map[string]string

	// The version of Kubernetes to run on the master and worker nodes and node pool (e.g. v1.11.9, v1.12.7)
	KubernetesVersion string

	// OCID of the cluster
	ClusterID string

	Network NetworkConfiguration
	// TODO we may want to support more than one node pool
	NodePool NodeConfiguration
	// cluster info
	ClusterInfo types.ClusterInfo
}

// Elements that make up the Network configuration (and state) for the OKE cluster
type NetworkConfiguration struct {
	// Optional pre-existing VCN in which you want to create cluster
	VCNName string
	// Optional pre-existing load balancer subnets to host load balancers for services
	ServiceLBSubnet1Name string
	ServiceLBSubnet2Name string
	// The number of subnets (each are created in different availability domains)
	QuantityOfSubnets int64
}

// Elements that make up the configuration of each node in the OKE cluster
type NodeConfiguration struct {
	// The OS image that will be used for the VM
	NodeImageName string
	// The shape of the VM for the worker node
	NodeShape string
	// The optional SSH Key to access the worker nodes
	NodeSSHKey string
	// The number of nodes in each subnet / availability domain
	QuantityPerSubnet int64
}

func NewDriver() types.Driver {
	driver := &Driver{
		driverCapabilities: types.Capabilities{
			Capabilities: make(map[int64]bool),
		},
	}

	driver.driverCapabilities.AddCapability(types.GetVersionCapability)
	driver.driverCapabilities.AddCapability(types.SetVersionCapability)
	driver.driverCapabilities.AddCapability(types.GetClusterSizeCapability)
	driver.driverCapabilities.AddCapability(types.SetClusterSizeCapability)

	return driver
}

func (d *Driver) Remove(ctx context.Context, info *types.ClusterInfo) error {
	logrus.Debugf("oke.driver.Remove(...) called")
	// Delete the cluster along with its node-pools and VCN (and associated network resource)

	state, err := GetState(info)
	if err != nil {
		return err
	}

	oke, err := GetClusterManagerClient(ctx, state)
	if err != nil {
		return errors.Wrap(err, "could not get container engine client")
	}

	nodePoolIDs, err := oke.ListNodepoolIdsInCluster(ctx, state.CompartmentID, state.ClusterID)
	if err != nil {
		return errors.Wrap(err, "could not retrieve node pool id")
	}

	for _, nodePoolID := range nodePoolIDs {
		logrus.Infof("Deleting node pool %s", nodePoolID)
		err := oke.DeleteNodePool(ctx, nodePoolID)
		if err != nil {
			return errors.Wrap(err, "could not delete node pool")
		}
	}

	// Get the VCN ID before the cluster is DELETED
	vcnID, err := oke.GetVcnByClusterID(ctx, state.ClusterID)
	if err != nil {
		return errors.Wrap(err, "could not retrieve virtual cloud network (VCN)")
	}

	logrus.Info("Deleting cluster")
	err = oke.DeleteCluster(ctx, state.ClusterID)
	if err != nil {
		return errors.Wrap(err, "could not delete cluster")
	}

	// TODO we could be deleting a preexisting VCN here
	logrus.Info("Deleting VCN")
	err = oke.DeleteVCN(ctx, vcnID)
	if err != nil {
		return errors.Wrap(err, "could not delete virtual cloud network (VCN)")
	}
	return nil
}

func GetState(info *types.ClusterInfo) (state, error) {
	logrus.Debugf("oke.driver.GetState(...) called")
	state := state{}
	err := json.Unmarshal([]byte(info.Metadata["state"]), &state)
	return state, err
}

// GetDriverCreateOptions implements driver interface
func (d *Driver) GetDriverCreateOptions(ctx context.Context) (*types.DriverFlags, error) {
	logrus.Debugf("oke.driver.GetDriverCreateOptions(...) called")

	driverFlag := types.DriverFlags{
		Options: make(map[string]*types.Flag),
	}
	driverFlag.Options["name"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The internal name of the Oracle Container Engine (OKE) cluster in Rancher",
	}
	driverFlag.Options["user-ocid"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The user Oracle Cloud ID (OCID) that has access the tenancy/compartment",
	}
	driverFlag.Options["tenancy-name"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The tenancy name for the OKE cluster",
	}
	driverFlag.Options["tenancy-id"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The OCID for the tenancy",
	}
	driverFlag.Options["fingerprint"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The API Key fingerprint for the OKE cluster",
	}
	driverFlag.Options["private-key-path"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The path to the private API Key that is associated with the user and has access the tenancy/compartment",
	}
	driverFlag.Options["private-key-contents"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The contents of the private API Key that is associated with the user and has access the tenancy/compartment",
	}
	driverFlag.Options["private-key-passphrase"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The passphrase of the private key for the OKE cluster",
	}
	driverFlag.Options["region"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The region where the OKE cluster will be hosted",
	}
	driverFlag.Options["availability-domain"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The availability domain within the region to host the OKE cluster",
	}
	driverFlag.Options["display-name"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The display name of the OKE cluster (and VCN and node pool if applicable) that should be displayed to the user",
	}
	driverFlag.Options["kubernetes-version"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The Kubernetes version that will be used for your master and worker nodes e.g. v1.11.9, v1.12.7",
	}
	driverFlag.Options["enable-kubernetes-dashboard"] = &types.Flag{
		Type:  types.BoolType,
		Usage: "Enable the kubernetes dashboard",
	}
	driverFlag.Options["enable-tiller"] = &types.Flag{
		Type:  types.BoolType,
		Usage: "Enable the kubernetes dashboard",
	}
	driverFlag.Options["node-ssh-key"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The SSH key for the worker nodes",
	}
	driverFlag.Options["quantity-of-node-subnets"] = &types.Flag{
		Type:  types.IntType,
		Usage: "Number of node subnets (defaults to one in each AD)",
	}
	driverFlag.Options["quantity-per-subnet"] = &types.Flag{
		Type:  types.IntType,
		Usage: "Number of worker nodes in each subnet / availability domain",
	}
	driverFlag.Options["wait-nodes-active"] = &types.Flag{
		Type:  types.IntType,
		Usage: "Whether to wait for the nodes to reach READY state before moving on",
	}
	driverFlag.Options["kubernetes-label"] = &types.Flag{
		Type:  types.StringSliceType,
		Usage: "The Kubernetes labels for the OKE cluster",
	}
	driverFlag.Options["node-image"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The OS for the node image",
	}
	driverFlag.Options["node-shape"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The shape of the node (determines number of CPUs and  amount of memory on each node)",
	}
	driverFlag.Options["compartment-id"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The compartment ID to use for the OKE cluster, node pool, and VCN",
	}
	driverFlag.Options["vcn-name"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The name of an existing virtual network to be used for OKE cluster creation",
	}
	driverFlag.Options["load-balancer-subnet-name-1"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The name of the first existing subnet to use for Kubernetes services / LB",
	}
	driverFlag.Options["load-balancer-subnet-name-2"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The name of the second existing subnet to use for Kubernetes services / LB",
	}
	driverFlag.Options["enable-private-nodes"] = &types.Flag{
		Type:  types.BoolType,
		Usage: "Whether worker nodes are deployed in private subnets",
	}

	return &driverFlag, nil
}

// GetDriverUpdateOptions implements driver interface
func (d *Driver) GetDriverUpdateOptions(ctx context.Context) (*types.DriverFlags, error) {
	logrus.Debugf("oke.driver.GetDriverUpdateOptions(...) called")

	driverFlag := types.DriverFlags{
		Options: make(map[string]*types.Flag),
	}
	driverFlag.Options["quantity-per-subnet"] = &types.Flag{
		Type:  types.IntType,
		Usage: "The updated number of worker nodes in each subnet to update. 1 (default) means no updates",
	}
	driverFlag.Options["kubernetes-version"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The updated Kubernetes version",
	}
	return &driverFlag, nil
}

// SetDriverOptions implements driver interface
func GetStateFromOpts(driverOptions *types.DriverOptions) (state, error) {
	logrus.Debugf("oke.driver.GetStateFromOpts(...) called")

	// Capture the requested options for the cluster
	state := state{
		ClusterInfo: types.ClusterInfo{
			Metadata: map[string]string{},
		},
		KubernetesLabels: map[string]string{},
	}

	state.CompartmentID = options.GetValueFromDriverOptions(driverOptions, types.StringType, "compartment-id", "CompartmentID").(string)
	state.Description = options.GetValueFromDriverOptions(driverOptions, types.StringType, "description").(string)
	state.EnableKubernetesDashboard = options.GetValueFromDriverOptions(driverOptions, types.BoolType, "enable-kubernetes-dashboard").(bool)
	state.EnableTiller = options.GetValueFromDriverOptions(driverOptions, types.BoolType, "enable-tiller").(bool)
	state.Fingerprint = options.GetValueFromDriverOptions(driverOptions, types.StringType, "fingerprint", "Fingerprint").(string)
	state.KubernetesVersion = options.GetValueFromDriverOptions(driverOptions, types.StringType, "kubernetes-version").(string)
	state.Name = options.GetValueFromDriverOptions(driverOptions, types.StringType, "name").(string)
	state.PrivateKeyContents = options.GetValueFromDriverOptions(driverOptions, types.StringType, "private-key-contents", "PrivateKeyContents").(string)
	state.PrivateKeyPath = options.GetValueFromDriverOptions(driverOptions, types.StringType, "private-key-path", "PrivateKeyPath").(string)
	state.PrivateKeyPassphrase = options.GetValueFromDriverOptions(driverOptions, types.StringType, "private-key-passphrase").(string)
	state.Region = options.GetValueFromDriverOptions(driverOptions, types.StringType, "region", "Region").(string)
	state.TenancyID = options.GetValueFromDriverOptions(driverOptions, types.StringType, "tenancy-id", "TenancyID").(string)
	state.UserOCID = options.GetValueFromDriverOptions(driverOptions, types.StringType, "user-ocid", "UserOCID").(string)
	state.WaitNodesActive = options.GetValueFromDriverOptions(driverOptions, types.IntType, "wait-nodes-active").(int64)
	state.PrivateNodes = options.GetValueFromDriverOptions(driverOptions, types.BoolType, "enable-private-nodes").(bool)

	state.NodePool = NodeConfiguration{
		NodeImageName:     options.GetValueFromDriverOptions(driverOptions, types.StringType, "node-image", "NodeImageName").(string),
		NodeShape:         options.GetValueFromDriverOptions(driverOptions, types.StringType, "node-shape", "NodeShape").(string),
		NodeSSHKey:        options.GetValueFromDriverOptions(driverOptions, types.StringType, "node-ssh-key").(string),
		QuantityPerSubnet: options.GetValueFromDriverOptions(driverOptions, types.IntType, "quantity-per-subnet").(int64),
	}

	state.Network = NetworkConfiguration{
		VCNName:              options.GetValueFromDriverOptions(driverOptions, types.StringType, "vcn-name").(string),
		ServiceLBSubnet1Name: options.GetValueFromDriverOptions(driverOptions, types.StringType, "load-balancer-subnet-name-1").(string),
		ServiceLBSubnet2Name: options.GetValueFromDriverOptions(driverOptions, types.StringType, "load-balancer-subnet-name-2").(string),
		QuantityOfSubnets:    options.GetValueFromDriverOptions(driverOptions, types.IntType, "quantity-of-node-subnets").(int64),
	}

	if state.NodePool.QuantityPerSubnet == 0 {
		state.NodePool.QuantityPerSubnet = defaultNumNodes
	}

	if state.PrivateKeyContents == "" && state.PrivateKeyPath != "" {
		privateKeyBytes, err := ioutil.ReadFile(state.PrivateKeyPath)
		if err == nil {
			state.PrivateKeyContents = string(privateKeyBytes)
		}
	}

	return state, state.validate()
}

func (s *state) validate() error {
	logrus.Debugf("oke.driver.validate(...) called")
	if s.PrivateKeyPath == "" && s.PrivateKeyContents == "" {
		return fmt.Errorf(`"private-key-path or private-key-contents" are required`)
	} else if s.TenancyID == "" {
		return fmt.Errorf(`"tenancy-id" is required`)
	} else if s.UserOCID == "" {
		return fmt.Errorf(`"user-ocid" is required`)
	} else if s.Fingerprint == "" {
		return fmt.Errorf(`"fingerprint" is required`)
	} else if s.CompartmentID == "" {
		return fmt.Errorf(`"compartment-id" is required`)
	} else if s.Region == "" {
		return fmt.Errorf(`"region" is required`)
	} else if s.NodePool.NodeImageName == "" {
		return fmt.Errorf(`"node-image " is required`)
	} else if s.NodePool.NodeShape == "" {
		return fmt.Errorf(`"node-shape " is required`)
	} else if s.Network.VCNName != "" && (s.Network.ServiceLBSubnet1Name == "" ||
		s.Network.ServiceLBSubnet2Name == "") {
		return fmt.Errorf(`"vcn-name", "load-balancer-subnet-name-1", and "load-balancer-subnet-name-2" must all be set together"`)
	}

	return nil
}

// Create implements driver interface
func (d *Driver) Create(ctx context.Context, opts *types.DriverOptions, _ *types.ClusterInfo) (*types.ClusterInfo, error) {
	logrus.Debugf("oke.driver.Create(...) called")

	state, err := GetStateFromOpts(opts)
	if err != nil {
		return nil, err
	}

	/*
	* The ClusterInfo includes the following information Version, ServiceAccountToken,Endpoint, username, password, etc
	 */
	clusterInfo := &types.ClusterInfo{}
	err = storeState(clusterInfo, state)
	if err != nil {
		return clusterInfo, err
	}

	oke, err := GetClusterManagerClient(ctx, state)
	if err != nil {
		return clusterInfo, err
	}

	var vcnID string
	var serviceSubnetIDs, nodeSubnetIds []string

	// Check if we need to create a new VCN or make use of an existing VCN and subnets.
	if state.Network.VCNName == "" {
		// Create a new Virtual Cloud Network with the default number of subnets
		state.Network.VCNName = defaultVCNNamePrefix + state.Name
		state.Network.ServiceLBSubnet1Name = defaultServiceSubnetPrefix + state.Name
		state.Network.ServiceLBSubnet2Name = defaultServiceSubnetPrefix + state.Name
		if state.Network.QuantityOfSubnets == 0 {
			state.Network.QuantityOfSubnets = defaultNumNodeSubnets
		}

		logrus.Info("creating VCN and required network resources")
		vcnID, serviceSubnetIDs, nodeSubnetIds, err = oke.CreateVCNAndNetworkResources(&state)

		if err != nil {
			logrus.Debugf("error creating the VCN and/or the required network resources %v", err)
			return clusterInfo, err
		}

	} else {
		// Use an existing VCN and subnets. Besides the VCN and subnets, we are
		// assuming that the internet gateway, route table, security lists are present and configured correctly.
		vcnID, err = oke.GetVcnByName(ctx, state.CompartmentID, state.Network.VCNName)
		if err != nil {
			logrus.Debugf("error looking up the Id of existing VCN %s %v", state.Network.VCNName, err)
			return clusterInfo, err
		}
		serviceSubnet1Id, err := oke.GetSubnetByName(ctx, state.CompartmentID, vcnID, state.Network.ServiceLBSubnet1Name)
		if err != nil {
			logrus.Debugf("error looking up the Id of a Kubernetes service Subnet %s %v", state.Network.ServiceLBSubnet1Name, err)
			return clusterInfo, err
		}
		serviceSubnetIDs = append(serviceSubnetIDs, serviceSubnet1Id)

		serviceSubnet2Id, err := oke.GetSubnetByName(ctx, state.CompartmentID, vcnID, state.Network.ServiceLBSubnet2Name)
		if err != nil {
			logrus.Debugf("error looking up the Id of a Kubernetes service Subnet %s %v", state.Network.ServiceLBSubnet2Name, err)
			return clusterInfo, err
		}
		serviceSubnetIDs = append(serviceSubnetIDs, serviceSubnet2Id)

		// First, get all the subnet Ids in the VCN, then remove the service subnets which should leave the node subnets
		nodeSubnetIds, _ = oke.ListSubnetIdsInVcn(ctx, state.CompartmentID, vcnID)
		for _, serviceSubnetID := range serviceSubnetIDs {
			for i, vcnSubnetID := range nodeSubnetIds {
				if serviceSubnetID == vcnSubnetID {
					nodeSubnetIds = remove(nodeSubnetIds, i)
				}
			}
		}

		// When using an existing VCN, we require at least three subnets for node pool.
		if len(nodeSubnetIds) < 3 {
			return clusterInfo, fmt.Errorf("your VCN must have at least three node subnets in different availability domains for node pool")
		}
		state.Network.QuantityOfSubnets = int64(len(nodeSubnetIds))
	}

	logrus.Info("Creating cluster")
	err = oke.CreateCluster(ctx, &state, vcnID, serviceSubnetIDs, nodeSubnetIds)
	_ = storeState(clusterInfo, state)
	if err != nil {
		logrus.Debugf("error creating the cluster %v", err)
		return clusterInfo, err
	}

	logrus.Info("Creating node pool")
	err = oke.CreateNodePool(ctx, &state, vcnID, serviceSubnetIDs, nodeSubnetIds)
	_ = storeState(clusterInfo, state)
	if err != nil {
		logrus.Debugf("error creating the node pool %v", err)
		return clusterInfo, err
	}

	err = storeState(clusterInfo, state)
	if err != nil {
		return clusterInfo, err
	}

	return clusterInfo, nil
}

// Update implements driver interface
func (d *Driver) Update(ctx context.Context, info *types.ClusterInfo, opts *types.DriverOptions) (*types.ClusterInfo, error) {
	logrus.Debugf("oke.driver.Update(...) called")
	state, err := GetState(info)
	if err != nil {
		return nil, err
	}

	newState, err := GetStateFromOpts(opts)
	if err != nil {
		return nil, err
	}

	if newState.NodePool.QuantityPerSubnet != state.NodePool.QuantityPerSubnet {
		logrus.Infof("Updating quantity of nodes per subnet to %d", uint64(newState.NodePool.QuantityPerSubnet))
		state.NodePool.QuantityPerSubnet = newState.NodePool.QuantityPerSubnet
	}
	if newState.KubernetesVersion != state.KubernetesVersion {
		logrus.Infof("Updating Kubernetes version to %s", newState.KubernetesVersion)
		state.KubernetesVersion = newState.KubernetesVersion
	}

	state.KubernetesVersion = newState.KubernetesVersion

	return info, storeState(info, state)
}

func (d *Driver) PostCheck(ctx context.Context, info *types.ClusterInfo) (*types.ClusterInfo, error) {
	logrus.Debugf("oke.driver.PostCheck(...) called")
	state, err := GetState(info)
	if err != nil {
		return nil, err
	}

	oke, err := GetClusterManagerClient(ctx, state)
	if err != nil {
		return nil, errors.Wrap(err, "could not get Oracle Container Engine client")
	}

	cluster, err := oke.GetClusterByID(ctx, state.ClusterID)
	if err != nil {
		return nil, errors.Wrap(err, "could not retrieve the cluster")
	}

	kubeConfig, _, err := oke.GetKubeconfigByClusterID(ctx, state.ClusterID)
	if err != nil {
		return nil, errors.Wrap(err, "could not retrieve the Oracle Container Engine kubeconfig")
	}

	info.Endpoint = *cluster.Endpoints.Kubernetes
	info.Version = *cluster.KubernetesVersion
	info.Username = ""
	info.Password = ""
	info.RootCaCertificate = kubeConfig.Clusters[0].Cluster.CertificateAuthorityData
	info.ClientCertificate = ""
	info.ClientKey = ""
	info.NodeCount = state.NodePool.QuantityPerSubnet * state.Network.QuantityOfSubnets
	info.Metadata["nodePool"] = state.Name + "-1"
	info.ServiceAccountToken = kubeConfig.Users[0].User.Token

	return info, nil
}

func (d *Driver) GetClusterSize(ctx context.Context, info *types.ClusterInfo) (*types.NodeCount, error) {
	logrus.Debugf("oke.driver.GetClusterSize(...) called")
	state, err := GetState(info)
	if err != nil {
		return nil, err
	}

	oke, err := GetClusterManagerClient(ctx, state)
	if err != nil {
		return nil, errors.Wrap(err, "could not get Oracle Container Engine client")
	}

	nodePoolIds, err := oke.ListNodepoolIdsInCluster(ctx, state.CompartmentID, state.ClusterID)
	if err != nil {
		return nil, errors.Wrap(err, "could not retrieve node pool id")
	}

	// Assumption of a single node pool here
	nodePool, err := oke.GetNodePoolByID(ctx, nodePoolIds[0])
	if err != nil {
		return nil, errors.Wrap(err, "could not retrieve node pool")
	}

	nodeCount := &types.NodeCount{Count: int64(len(nodePool.Nodes))}

	return nodeCount, nil
}

/*
* Marshal the Oracle Container Engine configuration state and store it in the types.ClusterInfo
 */
func storeState(info *types.ClusterInfo, state state) error {
	bytes, err := json.Marshal(state)
	if err != nil {
		return errors.Wrap(err, "could not marshal state")
	}

	if info.Metadata == nil {
		info.Metadata = map[string]string{}
	}
	info.Metadata["state"] = string(bytes)
	return nil
}

func (d *Driver) GetVersion(ctx context.Context, info *types.ClusterInfo) (*types.KubernetesVersion, error) {
	logrus.Debugf("oke.driver.GetVersion(...) called")
	state, err := GetState(info)
	if err != nil {
		return nil, err
	}

	oke, err := GetClusterManagerClient(ctx, state)
	if err != nil {
		return nil, errors.Wrap(err, "could not get Oracle Container Engine client")
	}

	cluster, err := oke.GetClusterByID(ctx, state.ClusterID)
	if err != nil {
		return nil, errors.Wrap(err, "could not retrieve the Oracle Container Engine cluster")
	}

	version := &types.KubernetesVersion{Version: *cluster.KubernetesVersion}

	return version, nil
}

func (d *Driver) SetClusterSize(ctx context.Context, info *types.ClusterInfo, count *types.NodeCount) error {
	logrus.Debugf("oke.driver.SetClusterSize(...) called")
	state, err := GetState(info)
	if err != nil {
		return err
	}

	oke, err := GetClusterManagerClient(ctx, state)
	if err != nil {
		return errors.Wrap(err, "could not get Oracle Container Engine client")
	}

	if count.Count%state.Network.QuantityOfSubnets != 0 {
		return fmt.Errorf("you must scale up/down node(s) in each subnet i.e. cluster-size must be an even multiple of %d", uint64(state.Network.QuantityOfSubnets))
	}

	desiredQtyPerSubnet := int(count.Count / state.Network.QuantityOfSubnets)

	logrus.Info("updating the number of nodes in each subnet")
	nodePoolIds, err := oke.ListNodepoolIdsInCluster(ctx, state.CompartmentID, state.ClusterID)
	if err != nil {
		return errors.Wrap(err, "could not retrieve node pool id")
	}

	// Assuming a single node pool
	nodePool, err := oke.GetNodePoolByID(ctx, nodePoolIds[0])
	if err != nil {
		return errors.Wrap(err, "could not retrieve node pool")
	}
	if *nodePool.QuantityPerSubnet == desiredQtyPerSubnet {
		logrus.Infof("cluster size is already %d (i.e. %d node(s) per subnet)", uint64(count.Count), desiredQtyPerSubnet)
		return nil
	}

	if *nodePool.QuantityPerSubnet == desiredQtyPerSubnet {
		logrus.Infof("cluster size is already %d (i.e. %d node(s) per subnet)", uint64(count.Count), desiredQtyPerSubnet)
		return nil
	}

	// Assuming a single node pool
	err = oke.ScaleNodePool(ctx, nodePoolIds[0], desiredQtyPerSubnet)
	if err != nil {
		return errors.Wrap(err, "could not adjust the number of nodes in the pool")
	}

	// scale is currently asynchronous
	logrus.Info("cluster size updated successfully")
	return nil
}

func (d *Driver) SetVersion(ctx context.Context, info *types.ClusterInfo, version *types.KubernetesVersion) error {
	logrus.Debugf("oke.driver.SetVersion(...) called")

	state, err := GetState(info)
	if err != nil {
		return err
	}

	oke, err := GetClusterManagerClient(ctx, state)
	if err != nil {
		return errors.Wrap(err, "could not get Oracle Container Engine client")
	}

	mErr := oke.UpdateMasterKubernetesVersion(ctx, state.ClusterID, version.Version)
	if mErr != nil {
		logrus.Debugf("warning: could not update the version of Kubernetes master(s)")
	}

	nodePoolIds, err := oke.ListNodepoolIdsInCluster(ctx, state.CompartmentID, state.ClusterID)
	if err != nil {
		return errors.Wrap(err, "could not retrieve node pool id")
	}

	var npErr error
	for _, nodePoolID := range nodePoolIds {
		nodePool, err := oke.GetNodePoolByID(ctx, nodePoolID)
		if err != nil {
			logrus.Debugf("could not retrieve node pool")
			continue
		}
		logrus.Infof("updating the version of Kubernetes to %s", version.Version)
		nextNpErr := oke.UpdateNodepoolKubernetesVersion(ctx, *nodePool.Id, version.Version)
		if nextNpErr != nil {
			npErr = nextNpErr
			logrus.Debugf("warning: could not update the version of Kubernetes master(s)")
		}
	}

	if mErr != nil {
		return errors.Wrap(mErr, "could not update the version of Kubernetes master(s)")
	} else if npErr != nil {
		return errors.Wrap(npErr, "could not update the Kubernetes version of one the node pool")
	}

	// update version is currently asynchronous
	logrus.Info("cluster version (masters and nodepools) updated successfully")
	return nil
}

func (d *Driver) GetCapabilities(ctx context.Context) (*types.Capabilities, error) {
	logrus.Debugf("oke.driver.GetCapabilities(...) called")
	return &d.driverCapabilities, nil
}

func (d *Driver) ETCDSave(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) error {
	logrus.Debugf("oke.driver.ETCDSave(...) called")
	return fmt.Errorf("ETCD backup operations are not implemented")
}

func (d *Driver) ETCDRestore(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) (*types.ClusterInfo, error) {
	logrus.Debugf("oke.driver.ETCDRestore(...) called")
	return nil, fmt.Errorf("ETCD backup operations are not implemented")
}

func (d *Driver) ETCDRemoveSnapshot(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) error {
	logrus.Debugf("oke.driver.ETCDRemoveSnapshot(...) called")
	return fmt.Errorf("ETCD backup operations are not implemented")
}

func (d *Driver) GetK8SCapabilities(ctx context.Context, options *types.DriverOptions) (*types.K8SCapabilities, error) {
	logrus.Debugf("oke.driver.GetK8SCapabilities(...) called")

	// TODO OCI supports persistent volumes as well
	capabilities := &types.K8SCapabilities{
		L4LoadBalancer: &types.LoadBalancerCapabilities{
			Enabled:              true,
			Provider:             "OCILB",
			ProtocolsSupported:   []string{"TCP", "HTTP/1.0", "HTTP/1.1"},
			HealthCheckSupported: true,
		},
	}

	return capabilities, nil
}

func (d *Driver) RemoveLegacyServiceAccount(ctx context.Context, info *types.ClusterInfo) error {
	logrus.Debugf("oke.driver.RemoveLegacyServiceAccount(...) called")
	// TODO
	return nil
}

// GetClusterManagerClient is a helper function that returns a new NewClusterManagerClient based on the state.
func GetClusterManagerClient(ctx context.Context, state state) (ClusterManagerClient, error) {
	configurationProvider := common.NewRawConfigurationProvider(
		state.TenancyID,
		state.UserOCID,
		state.Region,
		state.Fingerprint,
		state.PrivateKeyContents,
		&state.PrivateKeyPassphrase)

	clusterMgrClient, err := NewClusterManagerClient(configurationProvider)
	if err != nil {
		return *clusterMgrClient, nil
	}

	return *clusterMgrClient, nil
}

// remove is a helper function that removes an element at the specified index from a string array.
func remove(slice []string, s int) []string {
	return append(slice[:s], slice[s+1:]...)
}
