package oke

/*
ClusterManagerClient is a client for interacting with Oracle Cloud Engine API. It does all of the real work in
providing CRUD operations for the OKE cluster and VCN.
*/
import (
	"fmt"
	"github.com/oracle/oci-go-sdk/common"
	"github.com/oracle/oci-go-sdk/containerengine"
	"github.com/oracle/oci-go-sdk/core"
	"github.com/oracle/oci-go-sdk/example/helpers"
	"github.com/oracle/oci-go-sdk/identity"
	"github.com/rancher/kontainer-engine/store"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"math/rand"
	"strings"
	"time"

	//"github.com/rancher/kontainer-engine/types"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// TODO VCN block only needs to be large enough for five subnets below
	vcnCIDRBlock      = "10.0.0.0/16"
	node1CIDRBlock    = "10.0.10.0/24"
	node2CIDRBlock    = "10.0.11.0/24"
	node3CIDRBlock    = "10.0.12.0/24"
	service1CIDRBlock = "10.0.1.0/24"
	service2CIDRBlock = "10.0.2.0/24"
)

// Defines / contains the OCI/OKE/Identity clients and operations.
type ClusterManagerClient struct {
	configuration         common.ConfigurationProvider
	containerEngineClient containerengine.ContainerEngineClient
	virtualNetworkClient  core.VirtualNetworkClient
	identityClient        identity.IdentityClient
	// TODO we could also include the retry settings here
}

// NewClusterManagerClient creates a new cluster manager client.
func NewClusterManagerClient(configuration common.ConfigurationProvider) (*ClusterManagerClient, error) {
	containerClient, err := containerengine.NewContainerEngineClientWithConfigurationProvider(configuration)
	if err != nil {
		logrus.Debugf("create new ContainerEngine containerClient failed with err %v", err)
		return nil, err
	}
	vNetClient, err := core.NewVirtualNetworkClientWithConfigurationProvider(configuration)
	if err != nil {
		logrus.Debugf("create new VirtualNetwork client failed with err %v", err)
		return nil, err
	}
	identityClient, err := identity.NewIdentityClientWithConfigurationProvider(configuration)
	if err != nil {
		logrus.Debugf("create new Identity client failed with err %v", err)
		return nil, err
	}
	c := &ClusterManagerClient{
		configuration:         configuration,
		containerEngineClient: containerClient,
		virtualNetworkClient:  vNetClient,
		identityClient:        identityClient,
	}
	return c, nil
}

// CreateCluster creates a new cluster with no initial node pool and attaches it to the existing network resources, or an error.
// TODO stop passing in state
func (mgr *ClusterManagerClient) CreateCluster(ctx context.Context, state *state, vcnID string, serviceSubnetIds, nodeSubnetIds []string) error {
	if state == nil {
		return fmt.Errorf("valid state is required")
	}
	logrus.Debugf("creating cluster %s with VCN ID %s", state.Name, vcnID)

	if state.KubernetesVersion == "" {
		kubernetesVersion, err := getDefaultKubernetesVersion(mgr.containerEngineClient)
		if err != nil {
			return err
		} else if kubernetesVersion == nil {
			return fmt.Errorf("could not determine default Kubernetes version")
		}
		state.KubernetesVersion = *kubernetesVersion
	}

	cReq := containerengine.CreateClusterRequest{}
	cReq.Name = common.String(state.Name)
	cReq.CompartmentId = &state.CompartmentID
	cReq.VcnId = common.String(vcnID)
	cReq.KubernetesVersion = common.String(state.KubernetesVersion)
	cReq.Options = &containerengine.ClusterCreateOptions{
		ServiceLbSubnetIds: serviceSubnetIds,
		AddOns: &containerengine.AddOnOptions{
			IsKubernetesDashboardEnabled: common.Bool(state.EnableKubernetesDashboard),
			IsTillerEnabled:              common.Bool(state.EnableTiller),
		},
	}

	clusterResp, err := mgr.containerEngineClient.CreateCluster(ctx, cReq)
	if err != nil {
		return err
	}

	// wait until cluster creation work request complete
	logrus.Debugf("waiting for cluster %s to reach Active status..", state.Name)
	workReqRespCluster, err := waitUntilWorkRequestComplete(mgr.containerEngineClient, clusterResp.OpcWorkRequestId)
	if err != nil {
		logrus.Debugf("get work request failed with err %v", err)
		return err
	}

	clusterID := getResourceID(workReqRespCluster.Resources, containerengine.WorkRequestResourceActionTypeCreated, string(containerengine.ListWorkRequestsResourceTypeCluster))

	logrus.Debugf("clusterID: %s has been created", *clusterID)
	state.ClusterID = *clusterID

	return nil
}

// GetClusterByID returns the cluster with the specified Id, or an error
func (mgr *ClusterManagerClient) GetClusterByID(ctx context.Context, clusterID string) (containerengine.Cluster, error) {

	logrus.Debugf("getting cluster with Cluster ID %s", clusterID)

	if len(clusterID) == 0 {
		return containerengine.Cluster{}, fmt.Errorf("clusterID must be set to retrieve the cluster")
	}

	req := containerengine.GetClusterRequest{}
	req.ClusterId = common.String(clusterID)

	resp, err := mgr.containerEngineClient.GetCluster(ctx, req)
	if err != nil {
		logrus.Debugf("get cluster request failed with err %v", err)
		return containerengine.Cluster{}, err
	}

	return resp.Cluster, nil
}

// GetClusterByName returns the Cluster ID of the VCN with the specified name in the specified compartment or an error if it is not found.
func (mgr *ClusterManagerClient) GetClusterByName(ctx context.Context, compartmentID, name string) (string, error) {
	logrus.Debugf("getting cluster with name %s", name)

	if len(compartmentID) == 0 {
		return "", fmt.Errorf("compartmentID must be set to retrieve the cluster")
	} else if len(name) == 0 {
		return "", fmt.Errorf("name must be set to retrieve the cluster")
	}

	listClustersReq := containerengine.ListClustersRequest{}
	listClustersReq.CompartmentId = common.String(compartmentID)
	listClustersReq.Name = common.String(name)

	listClustersResp, err := mgr.containerEngineClient.ListClusters(ctx, listClustersReq)
	if err != nil {
		logrus.Debugf("list clusters failed with err %v", err)
		return "", err
	}
	for _, cluster := range listClustersResp.Items {
		if *cluster.Name == name {
			return *cluster.Id, nil
		}
	}

	return "", fmt.Errorf("%s not found", name)
}

// CreateNodePool creates a new node pool (i.e. a set of compute nodes) for the cluster, or an error.
// TODO stop passing in state
func (mgr *ClusterManagerClient) CreateNodePool(ctx context.Context, state *state, vcnID string, serviceSubnetIds, nodeSubnetIds []string) error {
	if state == nil {
		return fmt.Errorf("valid state is required")
	}
	logrus.Debugf("creating node pool %s with VCN ID %s", state.Name, vcnID)

	if state.KubernetesVersion == "" {
		kubernetesVersion, err := getDefaultKubernetesVersion(mgr.containerEngineClient)
		if err != nil {
			return err
		}
		state.KubernetesVersion = *kubernetesVersion
	}

	// Create a node pool for the cluster
	npReq := containerengine.CreateNodePoolRequest{}
	npReq.Name = common.String(state.Name + "-1")
	npReq.CompartmentId = common.String(state.CompartmentID)
	npReq.ClusterId = &state.ClusterID
	npReq.KubernetesVersion = &state.KubernetesVersion
	npReq.NodeImageName = common.String(state.NodePool.NodeImageName)
	npReq.NodeShape = common.String(state.NodePool.NodeShape)
	// Node-pool subnets used for node instances in the node pool.
	// These subnets should be different from the cluster Kubernetes Service LB subnets.
	npReq.SubnetIds = nodeSubnetIds
	npReq.InitialNodeLabels = []containerengine.KeyValue{{Key: common.String("driver"), Value: common.String("oraclekubernetesengine")}}
	if state.NodePool.NodeSSHKey != "" {
		npReq.SshPublicKey = common.String(state.NodePool.NodeSSHKey)
	}

	tmp := int(state.NodePool.QuantityPerSubnet)
	npReq.QuantityPerSubnet = &tmp

	createNodePoolResp, err := mgr.containerEngineClient.CreateNodePool(ctx, npReq)
	if err != nil {
		logrus.Debugf("create node pool request failed with err %v", err)
		return err
	}

	// wait until cluster creation work request complete
	logrus.Debugf("waiting for node pool to be created...")
	_, err = waitUntilWorkRequestComplete(mgr.containerEngineClient, createNodePoolResp.OpcWorkRequestId)
	if err != nil {
		logrus.Debugf("get work request failed with err %v", err)
		return err
	}

	return nil
}

// GetNodePoolByID returns the node pool with the specified Id, or an error.
func (mgr *ClusterManagerClient) GetNodePoolByID(ctx context.Context, nodePoolID string) (containerengine.NodePool, error) {

	logrus.Debugf("getting node pool with node pool ID %s", nodePoolID)

	if len(nodePoolID) == 0 {
		return containerengine.NodePool{}, fmt.Errorf("nodePoolID must be set to retrieve the node pool")
	}

	req := containerengine.GetNodePoolRequest{}
	req.NodePoolId = common.String(nodePoolID)

	resp, err := mgr.containerEngineClient.GetNodePool(ctx, req)
	if err != nil {
		logrus.Debugf("get node pool request failed with err %v", err)
		return containerengine.NodePool{}, err
	}

	return resp.NodePool, nil
}

// ScaleNodePool updates the number of nodes in each subnet, or an error.
func (mgr *ClusterManagerClient) ScaleNodePool(ctx context.Context, nodePoolID string, quantityPerSubnet int) error {
	logrus.Debugf("scaling node pool %s to %d nodes per subnet", nodePoolID, quantityPerSubnet)

	npReq := containerengine.UpdateNodePoolRequest{}
	npReq.NodePoolId = common.String(nodePoolID)
	npReq.QuantityPerSubnet = common.Int(quantityPerSubnet)

	_, err := mgr.containerEngineClient.UpdateNodePool(ctx, npReq)
	if err != nil {
		logrus.Debugf("scale node pool request failed with err %v", err)
		return err
	}

	// TODO consider optionally waiting until request is complete
	return nil
}

// UpdateKubernetesMasterVersion updates the version of Kubernetes on the master(s), or an error.
func (mgr *ClusterManagerClient) UpdateMasterKubernetesVersion(ctx context.Context, clusterID, version string) error {

	logrus.Debugf("updating master Kubernetes version of cluster ID %s to %s", clusterID, version)

	if len(clusterID) == 0 {
		return fmt.Errorf("clusterID must be set to upgrade the master(s)")
	}

	clReq := containerengine.UpdateClusterRequest{}
	clReq.ClusterId = common.String(clusterID)
	clReq.KubernetesVersion = common.String(version)

	cl, err := mgr.GetClusterByID(ctx, clusterID)
	if err == nil {
		logrus.Debugf("current Kubernetes version of cluster is %s", *cl.KubernetesVersion)
	}

	cpRes, err := mgr.containerEngineClient.UpdateCluster(ctx, clReq)
	if err != nil {
		logrus.Debugf("update Kubernetes version on cluster failed with err %v", err)
		return err
	}

	// wait until node pool deletion work request complete
	logrus.Debugf("waiting for cluster master version update...")
	_, err = waitUntilWorkRequestComplete(mgr.containerEngineClient, cpRes.OpcWorkRequestId)
	if err != nil {
		logrus.Debugf("get work request failed with err %v", err)
		return err
	}

	return nil
}

// UpdateNodepoolKubernetesVersion updates the version of Kubernetes on (new) worker that will be added to the node pool.
// Be sure to call UpdateKubernetesMasterVersion before updating the version of node pools, or an error.
func (mgr *ClusterManagerClient) UpdateNodepoolKubernetesVersion(ctx context.Context, nodePoolID, version string) error {

	logrus.Debugf("updating node pool Kubernetes version of node pool ID %s to %s", nodePoolID, version)

	if len(nodePoolID) == 0 {
		return fmt.Errorf("nodePoolID must be set to upgrade the node pool")
	}

	npReq := containerengine.UpdateNodePoolRequest{}
	npReq.NodePoolId = common.String(nodePoolID)
	npReq.KubernetesVersion = common.String(version)

	np, err := mgr.GetNodePoolByID(ctx, nodePoolID)
	if err == nil {
		logrus.Debugf("current Kubernetes version of node pool is %s", *np.KubernetesVersion)
	}

	// New nodes added to this node pool will run the updated version
	_, err = mgr.containerEngineClient.UpdateNodePool(ctx, npReq)
	if err != nil {
		logrus.Debugf("update Kubernetes version on node pool failed with err %v", err)
		return err
	}

	// TODO consider optionally waiting until request is complete
	return nil
}

// GetVcnByClusterID returns the VCN ID for the existing cluster with the specified Id, or an error.
func (mgr *ClusterManagerClient) GetVcnByClusterID(ctx context.Context, clusterID string) (string, error) {
	logrus.Debugf("getting cluster VCN with cluster ID %s", clusterID)

	cluster, err := mgr.GetClusterByID(ctx, clusterID)
	if err != nil {
		return "", err
	}

	return *cluster.VcnId, nil
}

// GetVcnByName returns the VCN ID of the VCN with the specified name in the specified compartment or an error if it is not found.
func (mgr *ClusterManagerClient) GetVcnByName(ctx context.Context, compartmentID, displayName string) (string, error) {
	logrus.Debugf("getting VCN with name %s", displayName)

	if len(compartmentID) == 0 {
		return "", fmt.Errorf("compartmentID must be set to retrieve its VCN")
	} else if len(displayName) == 0 {
		return "", fmt.Errorf("displayName must be set to retrieve its VCN")
	}

	listVcnsReq := core.ListVcnsRequest{}
	listVcnsReq.CompartmentId = common.String(compartmentID)
	listVcnsReq.DisplayName = common.String(displayName)

	listVcnsResp, err := mgr.virtualNetworkClient.ListVcns(ctx, listVcnsReq)
	if err != nil {
		logrus.Debugf("list VCNs failed with err %v", err)
		return "", err
	}
	for _, vcn := range listVcnsResp.Items {
		if *vcn.DisplayName == displayName {
			return *vcn.Id, nil
		}
	}

	return "", fmt.Errorf("%s not found", displayName)
}

// GetSubnetByName returns the subnet ID of the subnet with the specified name in the specified VCN and compartment, or an error if it is not found.
func (mgr *ClusterManagerClient) GetSubnetByName(ctx context.Context, compartmentID, vcnID, displayName string) (string, error) {
	logrus.Debugf("getting subnet with name %s", displayName)

	if len(compartmentID) == 0 {
		return "", fmt.Errorf("compartmentID must be set to get the subnet")
	} else if len(displayName) == 0 {
		return "", fmt.Errorf("displayName must be set to get the subnet")
	}

	listSubnetsReq := core.ListSubnetsRequest{}
	listSubnetsReq.CompartmentId = common.String(compartmentID)
	listSubnetsReq.VcnId = common.String(vcnID)
	listSubnetsReq.DisplayName = common.String(displayName)

	listSubnetsResp, err := mgr.virtualNetworkClient.ListSubnets(ctx, listSubnetsReq)
	if err != nil {
		logrus.Debugf("list subnets failed with err %v", err)
		return "", err
	}
	for _, subnet := range listSubnetsResp.Items {
		if *subnet.DisplayName == displayName {
			return *subnet.Id, nil
		}
	}

	return "", fmt.Errorf("%s not found", displayName)
}

// ListSubnetIdsInVcn returns the subnet IDs of any and all subnets in the specified VCN.
func (mgr *ClusterManagerClient) ListSubnetIdsInVcn(ctx context.Context, compartmentID, vcnID string) ([]string, error) {
	logrus.Debugf("list subnet Ids called")

	var ids []string

	if len(compartmentID) == 0 {
		return ids, fmt.Errorf("compartmentID must be set to list the subnets in the VCN")
	} else if len(vcnID) == 0 {
		return ids, fmt.Errorf("vcnID must be set to list the subnets in the VCN")
	}

	listSubnetsReq := core.ListSubnetsRequest{}
	listSubnetsReq.CompartmentId = common.String(compartmentID)
	listSubnetsReq.VcnId = common.String(vcnID)

	listSubnetsResp, err := mgr.virtualNetworkClient.ListSubnets(ctx, listSubnetsReq)
	if err != nil {
		logrus.Debugf("list subnets failed with err %v", err)
		return ids, err
	}
	for _, subnet := range listSubnetsResp.Items {
		ids = append(ids, *subnet.Id)
	}

	// subnet ids may be empty
	return ids, nil
}

// ListNodepoolIdsInCluster returns the node pool IDs of any and all node pools in the specified cluster.
func (mgr *ClusterManagerClient) ListNodepoolIdsInCluster(ctx context.Context, compartmentID, clusterID string) ([]string, error) {
	logrus.Debugf("list node pool ID(s) for cluster ID %s", clusterID)

	var ids []string

	if len(compartmentID) == 0 {
		return ids, fmt.Errorf("compartmentID must be set to list the node pools in the cluster")
	} else if len(clusterID) == 0 {
		return ids, fmt.Errorf("clusterID must be set to list the node pools in the cluster")
	}

	req := containerengine.ListNodePoolsRequest{}
	req.CompartmentId = common.String(compartmentID)
	req.ClusterId = common.String(clusterID)

	resp, err := mgr.containerEngineClient.ListNodePools(ctx, req)

	if err != nil {
		logrus.Debugf("list Node Pools request failed with err %v", err)
		return ids, err
	}

	for _, np := range resp.Items {
		ids = append(ids, *np.Id)
	}

	// subnet ids may be empty
	return ids, nil
}

// DeleteNodePool deletes the node pool with the specified ID, or an error
func (mgr *ClusterManagerClient) DeleteNodePool(ctx context.Context, nodePoolID string) error {
	logrus.Debugf("delete node pool with ID %s", nodePoolID)

	if len(nodePoolID) == 0 {
		return fmt.Errorf("nodePoolID must be set to delete the node pool")
	}

	req := containerengine.DeleteNodePoolRequest{}
	req.NodePoolId = common.String(nodePoolID)

	deleteNodePoolResp, err := mgr.containerEngineClient.DeleteNodePool(ctx, req)
	if err != nil {
		logrus.Debugf("delete node pool request failed with err %v", err)
		return err
	}

	// wait until node pool deletion work request complete
	logrus.Debugf("waiting for node pool to be deleted...")
	// TODO better to poll instead of sleep
	time.Sleep(10 * time.Second)
	_, err = waitUntilWorkRequestComplete(mgr.containerEngineClient, deleteNodePoolResp.OpcWorkRequestId)
	if err != nil {
		logrus.Debugf("get work request failed with err %v", err)
		return err
	}

	return nil
}

// DeleteCluster deletes the cluster with the specified ID, or it returns an error.
func (mgr *ClusterManagerClient) DeleteCluster(ctx context.Context, clusterID string) error {
	logrus.Debugf("deleting cluster with cluster ID %s", clusterID)

	if len(clusterID) == 0 {
		return fmt.Errorf("clusterID must be set to delete the cluster")
	}

	req := containerengine.DeleteClusterRequest{}
	req.ClusterId = common.String(clusterID)

	deleteClusterResp, err := mgr.containerEngineClient.DeleteCluster(ctx, req)
	if err != nil {
		logrus.Debugf("delete cluster request failed with err %v", err)
		return err
	}

	logrus.Debugf("waiting for cluster to be deleted...")
	// wait until cluster deletion work request complete
	_, err = waitUntilWorkRequestComplete(mgr.containerEngineClient, deleteClusterResp.OpcWorkRequestId)
	if err != nil {
		logrus.Debugf("get work request failed with err %v", err)
		return err
	}

	return nil
}

// DeleteVCN deletes the VCN and its associated resources (subnets, attached gateways, etc.) with the specified ID, or it returns an error.
func (mgr *ClusterManagerClient) DeleteVCN(ctx context.Context, vcnID string) error {

	logrus.Debugf("deleting VCN with VCN ID %s", vcnID)

	if len(vcnID) == 0 {
		return fmt.Errorf("vcnID must be set to delete the VCN")
	}

	getVCNReq := core.GetVcnRequest{}
	getVCNReq.VcnId = common.String(vcnID)

	getVCNResp, err := mgr.virtualNetworkClient.GetVcn(ctx, getVCNReq)
	if err != nil {
		logrus.Debugf("get VCN failed with err %v", err)
		return err
	}

	// The VCN must be completely empty (meaning no subnets, attached gateways, or security lists)

	// Delete Route Tables
	listRouteTblsReq := core.ListRouteTablesRequest{}
	listRouteTblsReq.VcnId = common.String(vcnID)
	listRouteTblsReq.CompartmentId = getVCNResp.Vcn.CompartmentId
	rtResp, err := mgr.virtualNetworkClient.ListRouteTables(ctx, listRouteTblsReq)
	if err != nil {
		logrus.Debugf("list route tables failed with err %v", err)
		return err
	}
	for _, rt := range rtResp.Items {

		updateRTReq := core.UpdateRouteTableRequest{}
		updateRTReq.RtId = rt.Id
		updateRTReq.RouteRules = []core.RouteRule{}

		logrus.Debugf("removing default route rule from route table %s", *rt.Id)
		_, err = mgr.virtualNetworkClient.UpdateRouteTable(ctx, updateRTReq)
		if err != nil {
			logrus.Debugf("update route table failed with err %v", err)
			return err
		}
	}

	// Delete Internet Gateways from VCN
	listIGsReq := core.ListInternetGatewaysRequest{}
	listIGsReq.VcnId = common.String(vcnID)
	listIGsReq.CompartmentId = getVCNResp.Vcn.CompartmentId
	igsResp, err := mgr.virtualNetworkClient.ListInternetGateways(ctx, listIGsReq)
	if err != nil {
		logrus.Debugf("list internet gateway(s) failed with err %v", err)
		return err
	}
	for _, ig := range igsResp.Items {
		deleteIGReq := core.DeleteInternetGatewayRequest{}
		deleteIGReq.IgId = ig.Id
		logrus.Debugf("deleting internet gateway %s", *ig.Id)
		_, err = mgr.virtualNetworkClient.DeleteInternetGateway(ctx, deleteIGReq)
		if err != nil {
			logrus.Debugf("warning: delete internet gateway failed with err %v", err)
			// Continue tearing down.
		}
	}

	// Delete all the subnets from VCN
	listSubnetsReq := core.ListSubnetsRequest{}
	listSubnetsReq.VcnId = common.String(vcnID)
	listSubnetsReq.CompartmentId = getVCNResp.Vcn.CompartmentId
	listSubnetResp, err := mgr.virtualNetworkClient.ListSubnets(ctx, listSubnetsReq)
	if err != nil {
		logrus.Debugf("list subnets failed with err %v", err)
		return err
	}
	for _, subnet := range listSubnetResp.Items {
		deleteSubnetReq := core.DeleteSubnetRequest{}
		deleteSubnetReq.SubnetId = subnet.Id
		logrus.Debugf("deleting subnet %s", *subnet.Id)
		_, err := mgr.virtualNetworkClient.DeleteSubnet(ctx, deleteSubnetReq)
		if err != nil {
			logrus.Debugf("warning: delete subnet failed with err %v", err)
			// Continue tearing down.
		}
	}
	// TODO better to poll instead of sleep
	time.Sleep(5 * time.Second)

	// Delete all security lists from VCN
	listSecurityListsReq := core.ListSecurityListsRequest{}
	listSecurityListsReq.VcnId = common.String(vcnID)
	listSecurityListsReq.CompartmentId = getVCNResp.Vcn.CompartmentId
	listSecurityListsResp, err := mgr.virtualNetworkClient.ListSecurityLists(ctx, listSecurityListsReq)
	if err != nil {
		logrus.Debugf("list security lists failed with err %v", err)
		return err
	}
	for _, securityList := range listSecurityListsResp.Items {
		deleteSecurityListReq := core.DeleteSecurityListRequest{}
		deleteSecurityListReq.SecurityListId = securityList.Id
		logrus.Debugf("deleting security list (%s)", *securityList.Id)
		_, err := mgr.virtualNetworkClient.DeleteSecurityList(ctx, deleteSecurityListReq)
		if err != nil {
			logrus.Debugf("warning: delete security list failed with err %v", err)
			// Continue tearing down.
		}
	}

	// Finally, delete the VCN itself
	vcnRequest := core.DeleteVcnRequest{}
	vcnRequest.VcnId = common.String(vcnID)

	logrus.Debugf("deleting VCN (%s)", common.String(vcnID))
	_, err = mgr.virtualNetworkClient.DeleteVcn(ctx, vcnRequest)
	if err != nil {
		logrus.Debugf("delete virtual-network request failed with err %v", err)
		return err
	}

	return nil
}

// GetKubeconfigByClusterID is a wrapper for the CreateKubeconfig operation that that handles errors and unmarshaling, or it returns an error.
func (mgr *ClusterManagerClient) GetKubeconfigByClusterID(ctx context.Context, clusterID string) (store.KubeConfig, string, error) {
	logrus.Debugf("getting KUBECONFIG with cluster ID %s", clusterID)

	kubeconfig := &store.KubeConfig{}

	if len(clusterID) == 0 {
		return store.KubeConfig{}, "", fmt.Errorf("clusterID must be set to get the KUBECONFIG file")
	}

	response, err := mgr.containerEngineClient.CreateKubeconfig(ctx, containerengine.CreateKubeconfigRequest{
		ClusterId: &clusterID,
	})
	if err != nil {
		logrus.Debugf("error creating kubeconfig %v", err)
		return store.KubeConfig{}, "", err
	}

	content, err := ioutil.ReadAll(response.Content)
	if err != nil {
		logrus.Debugf("error reading kubeconfig response content %v", err)
		return store.KubeConfig{}, "", err
	}

	err = yaml.Unmarshal(content, kubeconfig)
	if err != nil {
		logrus.Debugf("error unmarshalling kubeconfig %v", err)
		return store.KubeConfig{}, "", nil
	}

	return *kubeconfig, string(content), nil
}

// GetClientsetByClusterID returns a Kubernetes clientset for the specified cluster, or it returns an error.
func (mgr *ClusterManagerClient) GetClientsetByClusterID(ctx context.Context, clusterID string) (kubernetes.Interface, error) {
	logrus.Debugf("GetClientsetByClusterID called")

	kubeConfig, _, err := mgr.GetKubeconfigByClusterID(ctx, clusterID)
	if err != nil {
		return nil, err
	}

	if len(kubeConfig.Clusters) <= 0 {
		return nil, fmt.Errorf("there are no clusters in the cluster kubeconfig file")
	} else if len(kubeConfig.Users) <= 0 {
		return nil, fmt.Errorf("there are no users in the cluster kubeconfig file")
	}

	// in here we have to use http basic auth otherwise we can't get the permission to create cluster role
	config := &rest.Config{
		Host:        kubeConfig.Clusters[0].Cluster.Server,
		BearerToken: kubeConfig.Users[0].User.Token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: []byte(kubeConfig.Clusters[0].Cluster.CertificateAuthorityData),
		},
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

// CreateSubnets creates public node subnets in each availability domain, or it returns an error.
// TODO stop passing in state
func (mgr *ClusterManagerClient) CreateNodePublicSubnets(ctx context.Context, state *state, vcnID string, securityListIds []string) ([]string, error) {

	logrus.Debugf("creating (public) node subnet(s) in VCN ID %s", vcnID)

	var subnetIds = []string{}
	if state == nil {
		return subnetIds, fmt.Errorf("valid state is required")
	}

	// create a subnet in different availability domain
	req := identity.ListAvailabilityDomainsRequest{}
	req.CompartmentId = &state.CompartmentID
	ads, err := mgr.identityClient.ListAvailabilityDomains(ctx, req)
	if err != nil {
		return subnetIds, err
	}

	if len(ads.Items) < int(state.Network.QuantityOfSubnets) {
		return subnetIds, fmt.Errorf("there are less availability domains than required subnets")
	}

	if len(ads.Items) < 3 {
		return subnetIds, fmt.Errorf("VCN requires at least 3 (public) node subnets")
	}

	// Create each node subnet in a different availability domain
	nodeSubnetName := "nodedns1"
	availableDomain := ads.Items[0].Name
	subnet1, err := mgr.CreatePublicSubnetWithDetails(
		common.String(nodeSubnetName),
		common.String(node1CIDRBlock),
		common.String(nodeSubnetName),
		availableDomain,
		common.String(vcnID), securityListIds, state)
	if err != nil {
		logrus.Debugf("create new node subnet failed with err %v", err)
		return subnetIds, err
	}
	subnetIds = append(subnetIds, *subnet1.Id)

	nodeSubnetName = "nodedns2"
	availableDomain = ads.Items[1].Name
	subnet2, err := mgr.CreatePublicSubnetWithDetails(
		common.String(nodeSubnetName),
		common.String(node2CIDRBlock),
		common.String(nodeSubnetName),
		availableDomain,
		common.String(vcnID), securityListIds, state)
	if err != nil {
		logrus.Debugf("create new node subnet failed with err %v", err)
		return subnetIds, err
	}
	subnetIds = append(subnetIds, *subnet2.Id)

	nodeSubnetName = "nodedns3"
	availableDomain = ads.Items[2].Name
	subnet3, err := mgr.CreatePublicSubnetWithDetails(
		common.String(nodeSubnetName),
		common.String(node3CIDRBlock),
		common.String(nodeSubnetName),
		availableDomain,
		common.String(vcnID), securityListIds, state)
	if err != nil {
		logrus.Debugf("create new node subnet failed with err %v", err)
		return subnetIds, err
	}
	subnetIds = append(subnetIds, *subnet3.Id)

	return subnetIds, nil
}

// CreateSubnets creates the service subnets (load balancer subnets), or it returns an error.
// TODO stop passing in state
func (mgr *ClusterManagerClient) CreateServiceSubnets(ctx context.Context, state *state, vcnID string, securityListIds []string) ([]string, error) {
	logrus.Debugf("creating service / LB subnet(s) in VCN ID %s", vcnID)

	var subnetIds = []string{}
	if state == nil {
		return subnetIds, fmt.Errorf("valid state is required")
	}

	// create a subnet in different availability domain
	req := identity.ListAvailabilityDomainsRequest{}
	req.CompartmentId = &state.CompartmentID
	ads, err := mgr.identityClient.ListAvailabilityDomains(ctx, req)
	if err != nil {
		return subnetIds, err
	}

	if len(ads.Items) < 2 {
		return subnetIds, fmt.Errorf("at least 2 availability domains are required to host service subnets")
	}

	// Create each service subnet in a different availability domain
	availableDomain := ads.Items[1].Name
	subnet1, err := mgr.CreatePublicSubnetWithDetails(common.String(state.Network.ServiceLBSubnet1Name),
		common.String(service1CIDRBlock),
		common.String("svcdns1"),
		availableDomain,
		common.String(vcnID), securityListIds, state)
	if err != nil {
		logrus.Debugf("create new service subnet failed with err %v", err)
		return subnetIds, err
	}
	subnetIds = append(subnetIds, *subnet1.Id)

	availableDomain = ads.Items[2].Name

	subnet2, err := mgr.CreatePublicSubnetWithDetails(common.String(state.Network.ServiceLBSubnet2Name),
		common.String(service2CIDRBlock),
		common.String("svcdns2"),
		availableDomain,
		common.String(vcnID), securityListIds, state)
	if err != nil {
		logrus.Debugf("create new service / load balancer subnets failed with error %v", err)
		return subnetIds, err
	}
	subnetIds = append(subnetIds, *subnet2.Id)

	return subnetIds, nil
}

// CreatePublicSubnetWithDetails creates a new public subnet in the specified VCN, or it returns an error.
// TODO stop passing in state
func (mgr *ClusterManagerClient) CreatePublicSubnetWithDetails(displayName *string, cidrBlock *string, dnsLabel *string, availableDomain *string, vcnID *string, securityListIds []string, state *state) (core.Subnet, error) {

	if state == nil {
		return core.Subnet{}, fmt.Errorf("valid state is required")
	}

	ctx := context.Background()

	// create a new subnet
	request := core.CreateSubnetRequest{}
	request.AvailabilityDomain = availableDomain
	request.CompartmentId = &state.CompartmentID
	request.CidrBlock = cidrBlock
	request.DisplayName = displayName
	request.DnsLabel = dnsLabel
	request.VcnId = vcnID
	request.SecurityListIds = securityListIds

	request.RequestMetadata = helpers.GetRequestMetadataWithDefaultRetryPolicy()

	response, err := mgr.virtualNetworkClient.CreateSubnet(ctx, request)
	if err != nil {
		logrus.Debugf("create subnet request failed with err %v", err)
		return core.Subnet{}, err
	}

	// retry condition check, stop until return true
	pollUntilAvailable := func(r common.OCIOperationResponse) bool {
		if converted, ok := r.Response.(core.GetSubnetResponse); ok {
			return converted.LifecycleState != core.SubnetLifecycleStateAvailable
		}
		return true
	}

	pollGetRequest := core.GetSubnetRequest{
		SubnetId:        response.Id,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(pollUntilAvailable),
	}

	// wait for lifecycle become running
	_, pollErr := mgr.virtualNetworkClient.GetSubnet(ctx, pollGetRequest)
	helpers.FatalIfError(pollErr)

	return response.Subnet, nil
}

// CreateVCNAndNetworkResources creates a new Virtual Cloud Network and required resources including security lists,
// Internet Gateway, default route rule, etc., or it returns an error.
// TODO stop passing in state
func (mgr *ClusterManagerClient) CreateVCNAndNetworkResources(state *state) (string, []string, []string, error) {

	logrus.Debugf("create virtual cloud network called.")
	if state == nil {
		return "", nil, nil, fmt.Errorf("valid state is required")
	}

	ctx := context.Background()

	// create a new VCNID and sub-resources
	vcnRequest := core.CreateVcnRequest{}
	vcnRequest.CidrBlock = common.String(vcnCIDRBlock)
	vcnRequest.CompartmentId = &state.CompartmentID
	vcnRequest.DisplayName = common.String(state.Network.VCNName)
	dnsLabel := generateUniqueLabel("kontainer", 13)
	vcnRequest.DnsLabel = common.String(dnsLabel)

	r, err := mgr.virtualNetworkClient.CreateVcn(ctx, vcnRequest)
	if err != nil {
		logrus.Debugf("create virtual-network request failed with err %v", err)
		return "", nil, nil, err
	}
	// TODO better to poll instead of sleep
	time.Sleep(time.Second * 10)

	var trueVar = true
	// Create an internet gateway
	internetGatewayReq := core.CreateInternetGatewayRequest{
		CreateInternetGatewayDetails: core.CreateInternetGatewayDetails{
			CompartmentId: &state.CompartmentID,
			VcnId:         r.Vcn.Id,
			IsEnabled:     &trueVar,
			DisplayName:   common.String("IG"),
		}}

	igResp, err := mgr.virtualNetworkClient.CreateInternetGateway(ctx, internetGatewayReq)
	helpers.FatalIfError(err)

	routeTablesReq := core.ListRouteTablesRequest{}
	routeTablesReq.VcnId = r.Vcn.Id
	routeTablesReq.CompartmentId = common.String(state.CompartmentID)
	routeTablesResp, err := mgr.virtualNetworkClient.ListRouteTables(ctx, routeTablesReq)
	if err != nil {
		logrus.Debugf("list route tables request failed with err %v", err)
		return "", nil, nil, err
	}
	if len(routeTablesResp.Items) != 1 {
		return "", nil, nil, fmt.Errorf("cannot find default route rule for the VCN")
	}

	// Add a route rule in the route table that directs internet-bound traffic to the internet gateway created above.
	updateRouteTableReq := core.UpdateRouteTableRequest{}
	updateRouteTableReq.RtId = routeTablesResp.Items[0].Id
	updateRouteTableReq.DisplayName = routeTablesResp.Items[0].DisplayName
	updateRouteTableReq.RouteRules = append(updateRouteTableReq.RouteRules, core.RouteRule{Destination: common.String("0.0.0.0/0"), NetworkEntityId: igResp.InternetGateway.Id})
	_, err = mgr.virtualNetworkClient.UpdateRouteTable(ctx, updateRouteTableReq)
	if err != nil {
		logrus.Debugf("update route table request failed with err %v", err)
		return "", nil, nil, err
	}

	// Allow OKE incoming access worker nodes on port 22 for setup and maintenance
	okeCidrBlocks := []string{"130.35.0.0/16", "134.70.0.0/17", "138.1.0.0/16", "140.91.0.0/17", "147.154.0.0/16", "192.29.0.0/16"}
	okeAdminPortRange := core.PortRange{
		Max: common.Int(22),
		Min: common.Int(22),
	}

	nodeSecList := core.CreateSecurityListRequest{
		CreateSecurityListDetails: core.CreateSecurityListDetails{
			CompartmentId:        &state.CompartmentID,
			DisplayName:          common.String("Node Security List"),
			EgressSecurityRules:  []core.EgressSecurityRule{},
			IngressSecurityRules: []core.IngressSecurityRule{},
			VcnId:                r.Vcn.Id}}

	// Default egress rule to allow outbound traffic to the internet
	nodeSecList.EgressSecurityRules = append(nodeSecList.EgressSecurityRules, core.EgressSecurityRule{
		Protocol:    common.String("6"), // TCP
		Destination: common.String("0.0.0.0/0"),
	})
	// Allow internal traffic from other worker nodes by default
	nodeSecList.EgressSecurityRules = append(nodeSecList.EgressSecurityRules, core.EgressSecurityRule{
		Protocol:    common.String("all"),
		Destination: common.String(node1CIDRBlock),
	})
	nodeSecList.EgressSecurityRules = append(nodeSecList.EgressSecurityRules, core.EgressSecurityRule{
		Protocol:    common.String("all"),
		Destination: common.String(node2CIDRBlock),
	})
	nodeSecList.EgressSecurityRules = append(nodeSecList.EgressSecurityRules, core.EgressSecurityRule{
		Protocol:    common.String("all"),
		Destination: common.String(node3CIDRBlock),
	})

	for _, okeCidr := range okeCidrBlocks {
		nodeSecList.IngressSecurityRules = append(nodeSecList.IngressSecurityRules, core.IngressSecurityRule{
			Protocol: common.String("6"), // TCP
			Source:   common.String(okeCidr),
			TcpOptions: &core.TcpOptions{
				DestinationPortRange: &okeAdminPortRange,
			},
		})
	}
	// Allow internal traffic from other worker nodes by default
	nodeSecList.IngressSecurityRules = append(nodeSecList.IngressSecurityRules, core.IngressSecurityRule{
		Protocol: common.String("all"),
		Source:   common.String(node1CIDRBlock),
	})
	nodeSecList.IngressSecurityRules = append(nodeSecList.IngressSecurityRules, core.IngressSecurityRule{
		Protocol: common.String("all"),
		Source:   common.String(node2CIDRBlock),
	})
	nodeSecList.IngressSecurityRules = append(nodeSecList.IngressSecurityRules, core.IngressSecurityRule{
		Protocol: common.String("all"),
		Source:   common.String(node3CIDRBlock),
	})
	// Allow incoming traffic on standard node ports
	nodePortRange := core.PortRange{
		Max: common.Int(32767),
		Min: common.Int(30000),
	}
	nodeSecList.IngressSecurityRules = append(nodeSecList.IngressSecurityRules, core.IngressSecurityRule{
		Protocol: common.String("6"), // TCP
		Source:   common.String(service1CIDRBlock),
		TcpOptions: &core.TcpOptions{
			DestinationPortRange: &nodePortRange,
		},
	})
	nodeSecList.IngressSecurityRules = append(nodeSecList.IngressSecurityRules, core.IngressSecurityRule{
		Protocol: common.String("6"), // TCP
		Source:   common.String(service2CIDRBlock),
		TcpOptions: &core.TcpOptions{
			DestinationPortRange: &nodePortRange,
		},
	})

	// TODO consider enabling 0.0.0.0/0 ICMP for nodes to receive Path MTU Discovery fragmentation messages.

	secListResp, err := mgr.virtualNetworkClient.CreateSecurityList(ctx, nodeSecList)
	helpers.FatalIfError(err)

	nodeSubnets, err := mgr.CreateNodePublicSubnets(ctx, state, *r.Vcn.Id, []string{*secListResp.SecurityList.Id})
	helpers.FatalIfError(err)

	serviceSubnets, err := mgr.CreateServiceSubnets(ctx, state, *r.Vcn.Id, []string{})
	helpers.FatalIfError(err)

	return *r.Vcn.Id, serviceSubnets, nodeSubnets, nil
}

// getResourceID returns a resource ID based on the filter of resource actionType and entityType
func getResourceID(resources []containerengine.WorkRequestResource, actionType containerengine.WorkRequestResourceActionTypeEnum, entityType string) *string {

	for _, resource := range resources {
		if resource.ActionType == actionType && strings.ToUpper(*resource.EntityType) == entityType {
			return resource.Identifier
		}
	}

	return nil
}

// wait until work request finish
func waitUntilWorkRequestComplete(client containerengine.ContainerEngineClient, workRequestID *string) (containerengine.GetWorkRequestResponse, error) {
	// TODO - this function seems to be taking too long and not returning as soon as the job appears to be complete.

	if workRequestID == nil || len(*workRequestID) == 0 {
		return containerengine.GetWorkRequestResponse{}, fmt.Errorf("a valid workRequestID is required")
	}

	// retry GetWorkRequest call until TimeFinished is set
	shouldRetryFunc := func(r common.OCIOperationResponse) bool {
		return r.Response.(containerengine.GetWorkRequestResponse).TimeFinished == nil
	}

	getWorkReq := containerengine.GetWorkRequestRequest{
		WorkRequestId:   workRequestID,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(shouldRetryFunc),
	}

	getResp, err := client.GetWorkRequest(context.Background(), getWorkReq)
	if err != nil {
		return getResp, err
	}

	return getResp, nil
}

func getDefaultKubernetesVersion(client containerengine.ContainerEngineClient) (*string, error) {

	getClusterOptionsReq := containerengine.GetClusterOptionsRequest{
		ClusterOptionId: common.String("all"),
	}
	getClusterOptionsResp, err := client.GetClusterOptions(context.Background(), getClusterOptionsReq)
	if err != nil {
		return nil, err
	}

	kubernetesVersion := getClusterOptionsResp.KubernetesVersions

	if len(kubernetesVersion) < 1 {
		return nil, fmt.Errorf("no Kubernetes versions are available")
	}

	return &kubernetesVersion[len(kubernetesVersion)-1], nil
}

func generateUniqueLabel(prefix string, length int) string {
	bytes := make([]byte, length)
	for i := 0; i < length; i++ {
		bytes[i] = byte(randInt(65, 90))
	}
	return (prefix + string(bytes))[0:length]
}

func randInt(min int, max int) int {
	return min + rand.Intn(max-min)
}
