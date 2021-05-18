package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

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

	"github.com/ironcladlou/hypershift-hacks/panopticon/web"
)

func main() {
	cmd := &cobra.Command{
		Use:          "panopticon",
		Short:        "Command for inspecting hosted clusters",
		SilenceUsage: true,
	}

	cmd.AddCommand(NewServeCommand())
	cmd.AddCommand(newRenderCommand())

	if err := cmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

type ServeOptions struct {
	Namespace   string
	Name        string
	Addr        string
	Development bool
}

func NewServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serves an API for working with clusters",
		SilenceUsage: true,
	}
	opts := &ServeOptions{
		Namespace:   "ci",
		Name:        "provider",
		Addr:        "localhost:8080",
		Development: false,
	}
	cmd.Flags().StringVar(&opts.Namespace, "namespace", opts.Namespace, "The root cluster namespace")
	cmd.Flags().StringVar(&opts.Name, "name", opts.Name, "The root cluster name")
	cmd.Flags().StringVar(&opts.Addr, "addr", opts.Addr, "The serving address")
	cmd.Flags().BoolVar(&opts.Development, "development", opts.Development, "If enabled, serve web from disk instead of the embedded binary contents")
	cmd.Run = func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		go func() {
			<-sigs
			cancel()
		}()

		server := &AnalyzerServer{}
		if err := server.Run(ctx, opts); err != nil {
			log.Fatal(err)
		}
	}
	return cmd
}

type AnalyzerServer struct {
	analyzer *Analyzer
}

func (s *AnalyzerServer) Run(ctx context.Context, opts *ServeOptions) error {
	restConfig := ctrl.GetConfigOrDie()
	restConfig.Burst = 300
	c, err := client.New(restConfig, client.Options{Scheme: hyperapi.Scheme})
	if err != nil {
		return err
	}

	s.analyzer = &Analyzer{
		Client: c,
	}
	s.analyzer.ScheduleUpdates(ctx, opts.Namespace, opts.Name, 30*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/clusters", s.handleGetClusters)

	var webFS fs.FS
	if opts.Development {
		if pwd, err := os.Getwd(); err != nil {
			return err
		} else {
			dir := filepath.Join(pwd, "/panopticon/web")
			log.Printf("Serving web contents from %s\n", dir)
			webFS = os.DirFS(dir)
		}
	} else {
		log.Println("Serving web contents from embedded binary")
		webFS = web.Assets
	}

	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		tmpl, err := template.ParseFS(webFS, "index.tmpl")
		if err != nil {
			log.Println(err)
			return
		}
		if err := tmpl.Execute(w, s.analyzer.model.Root); err != nil {
			log.Println(err)
		}
	}))

	server := http.Server{Addr: opts.Addr, Handler: mux}
	go func() {
		<-ctx.Done()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("error shutting down server: %s", err)
		}
	}()
	log.Printf("Serving on %s", opts.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *AnalyzerServer) handleGetClusters(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.analyzer.GetModel().Root)
}

type Analyzer struct {
	Client client.Client

	model *AnalysisModel
	mu    sync.RWMutex
}

func (s *Analyzer) ScheduleUpdates(ctx context.Context, namespace string, name string, interval time.Duration) {
	t := time.NewTicker(interval)
	update := make(chan struct{}, 1)
	update <- struct{}{}
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				update <- struct{}{}
			}
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-update:
				if err := s.Update(ctx, namespace, name); err != nil {
					log.Printf("error updating model: %s", err)
				}
			}
		}
	}()
}

func (s *Analyzer) Update(ctx context.Context, namespace string, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Println("updating model")
	var rootCluster hyperv1.HostedCluster
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &rootCluster); err != nil {
		return fmt.Errorf("error updating model: %w", err)
	}
	rootNode, err := collectNodes(ctx, s.Client, &rootCluster, nil)
	if err != nil {
		return fmt.Errorf("error analyzing clusters: %w", err)
	}
	s.model = &AnalysisModel{
		Root:        rootNode,
		LastUpdated: time.Now(),
	}
	log.Println("finished updating model")
	return nil
}

func (s *Analyzer) GetModel() *AnalysisModel {
	return s.model
}

type AnalysisModel struct {
	Root        *Cluster  `json:"root"`
	LastUpdated time.Time `json:"lastUpdated"`
}

type Cluster struct {
	Resource *hyperv1.HostedCluster `json:"resource"`

	LastUpdated       time.Time `json:"lastObserved"`
	KubeConfig        string    `json:"kubeConfig"`
	ConsoleURL        string    `json:"consoleURL"`
	KubeadminPassword string    `json:"kubeadminPassword"`
	Errors            []string  `json:"errors"`

	Children []*Cluster `json:"children"`

	mu sync.Mutex
}

func (c *Cluster) RecordError(err error) {
	c.mu.Lock()
	c.Errors = append(c.Errors, err.Error())
	c.mu.Unlock()
}

// This should accumulate errors along the way on the node and only return an error
// if the entire traversal should be halted.
func collectNodes(ctx context.Context, providerClient client.Client, cluster *hyperv1.HostedCluster, parent *hyperv1.HostedCluster) (*Cluster, error) {
	parentName := "<none>"
	if parent != nil {
		parentName = fmt.Sprintf("%s/%s", parent.Namespace, parent.Name)
	}
	log.Printf("processing cluster %s/%s descended from %s", cluster.Namespace, cluster.Name, parentName)

	node := &Cluster{
		LastUpdated: time.Now(),
		Children:    []*Cluster{},
		Errors:      []string{},
	}

	clusterClone := cluster.DeepCopy()
	clusterClone.ManagedFields = nil
	node.Resource = clusterClone

	if kubeConfigData, hasKubeConfigData, err := getKubeConfig(ctx, providerClient, cluster); err != nil {
		node.RecordError(fmt.Errorf("failed to get kubeconfig: %w", err))
	} else {
		if hasKubeConfigData && len(kubeConfigData) > 0 {
			node.KubeConfig = string(kubeConfigData)
		} else {
			// If we have no kubeconfig, we can't make a client to do any further
			// processing, so return early
			node.RecordError(fmt.Errorf("no kubeconfig data found"))
			return node, nil
		}
	}

	// If we can't get a client to the control plane, return early because there
	// no way to dive deeper.
	var controlPlaneClient client.Client
	if controlPlaneKubeConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(node.KubeConfig)); err != nil {
		node.RecordError(fmt.Errorf("failed to make kube config: %w", err))
		return node, nil
	} else {
		controlPlaneKubeConfig.Burst = 300
		c, err := client.New(controlPlaneKubeConfig, client.Options{Scheme: hyperapi.Scheme})
		if err != nil {
			node.RecordError(fmt.Errorf("failed to make control plane kube client: %w", err))
			return node, nil
		} else {
			controlPlaneClient = c
		}
	}

	if consoleURL, err := getConsoleURL(ctx, controlPlaneClient); err != nil {
		node.RecordError(fmt.Errorf("failed to get console URL: %w", err))
	} else {
		node.ConsoleURL = consoleURL
	}

	if kubeadminPassword, hasKubeadminPassword, err := getConsoleCredentials(ctx, providerClient, cluster); err != nil {
		node.RecordError(fmt.Errorf("failed to get console credentials: %w", err))
	} else {
		if hasKubeadminPassword {
			node.KubeadminPassword = string(kubeadminPassword)
		} else {
			node.RecordError(fmt.Errorf("no kubeadmin password found"))
		}
	}

	var childClusters hyperv1.HostedClusterList
	if err := controlPlaneClient.List(ctx, &childClusters); err != nil {
		node.RecordError(fmt.Errorf("failed to list child clusters: %w", err))
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
