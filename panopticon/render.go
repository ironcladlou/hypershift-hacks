package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperapi "github.com/openshift/hypershift/api"
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
)

type RenderOptions struct {
	Namespace string
	Name      string
}

func newRenderCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "render",
		Short:        "Renders a tree of hosted clusters",
		SilenceUsage: true,
	}
	opts := &RenderOptions{
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
	return cmd
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
