package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperapi "github.com/openshift/hypershift/api"
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
)

type Options struct {
	Namespace string
	Name      string
}

func main() {
	cmd := &cobra.Command{
		Use:          "tree",
		Short:        "Creates a tree of hosted clusters",
		SilenceUsage: true,
	}

	opts := &Options{
		Namespace: "ci",
		Name:      "provider",
	}

	cmd.Flags().StringVar(&opts.Namespace, "namespace", opts.Namespace, "The root cluster namespace")
	cmd.Flags().StringVar(&opts.Name, "name", opts.Name, "The root cluster name")

	cmd.Run = func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		go func() {
			<-sigs
			cancel()
		}()
		if err := render(ctx, opts.Namespace, opts.Name); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func render(ctx context.Context, namespace, name string) error {
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: hyperapi.Scheme})
	if err != nil {
		return err
	}

	var rootCluster hyperv1.HostedCluster
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &rootCluster); err != nil {
		return err
	}
	tree, err := collectNodes(ctx, c, &rootCluster, nil)
	if err != nil {
		return fmt.Errorf("failed to make tree: %w", err)
	}

	output, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("failed to marshal tree: %w", err)
	}
	fmt.Println(string(output))
	return nil
}

type Node struct {
	HostedCluster     *hyperv1.HostedCluster `json:"hostedCluster"`
	KubeConfig        []byte                 `json:"kubeConfig"`
	ConsoleURL        string                 `json:"consoleURL"`
	KubeAdminPassword []byte                 `json:"kubeAdminPassword"`

	Children []*Node `json:"children"`

	Errors []string `json:"errors"`
}

// This should accumulate errors along the way on the node and only return an error
// if the entire traversal should be halted.
func collectNodes(ctx context.Context, providerClient client.Client, cluster *hyperv1.HostedCluster, parent *hyperv1.HostedCluster) (*Node, error) {
	parentName := "<none>"
	if parent != nil {
		parentName = fmt.Sprintf("%s/%s", parent.Namespace, parent.Name)
	}
	log.Printf("processing cluster %s/%s descended from %s", cluster.Namespace, cluster.Name, parentName)

	node := &Node{
		Children: []*Node{},
		Errors:   []string{},
	}

	clusterClone := cluster.DeepCopy()
	clusterClone.ManagedFields = nil
	node.HostedCluster = clusterClone

	if kubeConfigData, hasKubeConfigData, err := getKubeConfig(ctx, providerClient, cluster); err != nil {
		node.Errors = append(node.Errors, fmt.Errorf("failed to get kubeconfig: %w", err).Error())
	} else {
		if hasKubeConfigData {
			node.KubeConfig = kubeConfigData
		} else {
			node.Errors = append(node.Errors, fmt.Errorf("no kubeconfig data found").Error())
		}
	}

	// If we can't get a client to the control plane, return early because there
	// no way to dive deeper.
	var controlPlaneClient client.Client
	if controlPlaneKubeConfig, err := clientcmd.RESTConfigFromKubeConfig(node.KubeConfig); err != nil {
		node.Errors = append(node.Errors, fmt.Errorf("failed to make kube config: %w", err).Error())
		return node, nil
	} else {
		c, err := client.New(controlPlaneKubeConfig, client.Options{Scheme: hyperapi.Scheme})
		if err != nil {
			node.Errors = append(node.Errors, fmt.Errorf("failed to make control plane kube client: %w", err).Error())
			log.Printf("err: %s", err)
			return node, nil
		} else {
			controlPlaneClient = c
		}
	}

	if consoleURL, err := getConsoleURL(ctx, controlPlaneClient); err != nil {
		node.Errors = append(node.Errors, fmt.Errorf("failed to get console URL: %w", err).Error())
	} else {
		node.ConsoleURL = consoleURL
	}

	if kubeAdminPassword, hasKubeAdminPassword, err := getConsoleCredentials(ctx, providerClient, cluster); err != nil {
		node.Errors = append(node.Errors, fmt.Errorf("failed to get console credentials: %w", err).Error())
	} else {
		if hasKubeAdminPassword {
			node.KubeAdminPassword = kubeAdminPassword
		} else {
			node.Errors = append(node.Errors, fmt.Errorf("no kubeadmin password found").Error())
		}
	}

	var childClusters hyperv1.HostedClusterList
	if err := controlPlaneClient.List(ctx, &childClusters); err != nil {
		node.Errors = append(node.Errors, fmt.Errorf("failed to list child clusters: %w", err).Error())
		return node, nil
	}
	for i := range childClusters.Items {
		childCluster := &childClusters.Items[i]
		child, err := collectNodes(ctx, controlPlaneClient, childCluster, cluster)
		if err != nil {
			return nil, fmt.Errorf("failed to inspect child cluster: %w", err)
		}
		node.Children = append(node.Children, child)
	}
	return node, nil
}

// Expects a provider client
func getKubeConfig(ctx context.Context, c client.Client, cluster *hyperv1.HostedCluster) ([]byte, bool, error) {
	if cluster.Status.KubeConfig == nil {
		return nil, false, nil
	}
	kubeConfigSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      cluster.Status.KubeConfig.Name,
		},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&kubeConfigSecret), &kubeConfigSecret); err != nil {
		return nil, false, fmt.Errorf("failed to get kubeconfig secret %s/%s: %w", kubeConfigSecret.Namespace, kubeConfigSecret.Name, err)
	}
	data, hasData := kubeConfigSecret.Data["kubeconfig"]
	if !hasData || len(data) == 0 {
		return nil, false, fmt.Errorf("kubeconfig secret %s/%s has no kubeconfig key", kubeConfigSecret.Namespace, kubeConfigSecret.Name)
	}
	return data, true, nil
}

// Expects a provider client
func getConsoleCredentials(ctx context.Context, c client.Client, cluster *hyperv1.HostedCluster) ([]byte, bool, error) {
	controlPlaneNamespace := manifests.HostedControlPlaneNamespace(cluster.Namespace, cluster.Name)

	kubeAdmin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: controlPlaneNamespace.Name,
			Name:      "kubeadmin-password",
		},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(kubeAdmin), kubeAdmin); err != nil {
		return nil, false, fmt.Errorf("failed to get kubeadmin-password secret %s: %w", client.ObjectKeyFromObject(kubeAdmin), err)
	}

	data, hasData := kubeAdmin.Data["password"]
	if !hasData || len(data) == 0 {
		return nil, false, fmt.Errorf("kubeadmin secret has no password")
	}

	return data, true, nil
}

// Expects a control plane client
func getConsoleURL(ctx context.Context, c client.Client) (string, error) {
	consoleRoute := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "openshift-console",
			Name:      "console",
		},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(consoleRoute), consoleRoute); err != nil {
		return "", fmt.Errorf("failed to get console route: %w", err)
	}
	if len(consoleRoute.Status.Ingress) == 0 {
		return "", fmt.Errorf("console route is not reporting an ingress")
	}
	return fmt.Sprintf("https://" + consoleRoute.Status.Ingress[0].Host), nil
}
