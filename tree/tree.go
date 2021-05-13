package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperapi "github.com/openshift/hypershift/api"
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
	"github.com/openshift/hypershift/cmd/util"
)

func main() {
	cmd := &cobra.Command{
		Use:          "tree",
		Short:        "Creates a tree of hosted clusters",
		SilenceUsage: true,
	}

	cmd.Run = func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		go func() {
			<-sigs
			cancel()
		}()
		if err := render(ctx); err != nil {
			os.Exit(1)
		}
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func render(ctx context.Context) error {
	scheme := runtime.NewScheme()
	if err := clientcmdapiv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to set up scheme: %w", err)
	}
	c := util.GetClientOrDie()
	var rootCluster hyperv1.HostedCluster
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ci", Name: "provider"}, &rootCluster); err != nil {
		return err
	}
	tree, err := buildTree(ctx, c, &rootCluster)
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
	HostedCluster *hyperv1.HostedCluster
	KubeConfig    []byte

	Children []*Node
}

func buildTree(ctx context.Context, parentClient client.Client, cluster *hyperv1.HostedCluster) (*Node, error) {
	cluster = cluster.DeepCopy()
	cluster.ManagedFields = nil
	node := &Node{
		HostedCluster: cluster,
		Children:      []*Node{},
	}
	kubeConfigData, _, _ := getKubeConfig(ctx, parentClient, cluster)
	node.KubeConfig = kubeConfigData
	kubeConfig, _ := clientcmd.RESTConfigFromKubeConfig(kubeConfigData)
	clusterClient, _ := client.New(kubeConfig, client.Options{Scheme: hyperapi.Scheme})
	var childClusters hyperv1.HostedClusterList
	clusterClient.List(ctx, &childClusters)
	for i := range childClusters.Items {
		childCluster := &childClusters.Items[i]
		child, _ := buildTree(ctx, clusterClient, childCluster)
		node.Children = append(node.Children, child)
	}
	return node, nil
}

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
