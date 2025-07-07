package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating clientset: %s", err.Error())
	}

	// Read the YAML file
	yamlFile, err := ioutil.ReadFile("manifests/raycluster.yaml")
	if err != nil {
		log.Fatalf("Error reading YAML file: %s", err.Error())
	}

	// Decode YAML to get object details
	dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	obj := &unstructured.Unstructured{}
	_, _, err = dec.Decode(yamlFile, nil, obj)
	if err != nil {
		log.Fatalf("Error decoding YAML: %s", err.Error())
	}
	namespace := obj.GetNamespace()
	if namespace == "" {
		namespace = "default"
	}
	name := obj.GetName()

	for i := 0; i < 10; i++ {
		log.Printf("Iteration %d", i+1)

		// Apply the YAML
		err = ApplyYaml(context.Background(), config, clientset, yamlFile)
		if err != nil {
			log.Fatalf("Error applying YAML: %s", err.Error())
		}

		log.Printf("Successfully applied manifests/raycluster.yaml: %s/%s", namespace, name)

		// Create dynamic client for fetching the applied resource
		dyn, err := dynamic.NewForConfig(config)
		if err != nil {
			log.Fatalf("Error creating dynamic client: %s", err.Error())
		}

		// Poll for readiness duration
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		rayClusterGVR := schema.GroupVersionResource{
			Group:    "ray.io",
			Version:  "v1",
			Resource: "rayclusters",
		}

		watcher, err := dyn.Resource(rayClusterGVR).Namespace(namespace).Watch(ctx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + name,
		})
		if err != nil {
			log.Fatalf("Failed to create watcher: %v", err)
		}
		defer watcher.Stop()

		var creationTime time.Time
	L:
		for {
			select {
			case <-ctx.Done():
				log.Fatalf("Timed out waiting for RayCluster %s to become ready", name)
			case event := <-watcher.ResultChan():
				unstructuredObj, ok := event.Object.(*unstructured.Unstructured)
				if !ok {
					log.Printf("unexpected type %T, skipping", event.Object)
					continue
				}
				if creationTime.IsZero() {
					creationTime = unstructuredObj.GetCreationTimestamp().Time
					if creationTime.IsZero() {
						continue // Wait for creation timestamp to be set
					}
				}
				readyTimeStr, found, err := unstructured.NestedString(unstructuredObj.Object, "status", "stateTransitionTimes", "ready")
				if err != nil || !found {
					continue // Not ready yet, continue polling
				}
				readyTime, err := time.Parse(time.RFC3339, readyTimeStr)
				if err != nil {
					log.Printf("Error parsing time: %v", err)
					continue
				}
				log.Printf("RayCluster '%s' took %s to become ready.", name, readyTime.Sub(creationTime))
				break L
			}
		}

		// Delete the RayCluster
		err = deleteRayCluster(context.Background(), config, yamlFile)
		if err != nil {
			log.Fatalf("Error deleting RayCluster: %s", err.Error())
		}
		log.Printf("Successfully deleted RayCluster: %s/%s", namespace, name)

		// Wait a bit before the next iteration
		time.Sleep(10 * time.Second)
	}
}

// getRayClusterReadyDuration fetches the specified RayCluster and calculates how long it took
// for it to become 'ready' by comparing the creationTimestamp to the 'ready' stateTransitionTime.
func ApplyYaml(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, yamlContent []byte) error {
	dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	obj := &unstructured.Unstructured{}
	_, gvk, err := dec.Decode(yamlContent, nil, obj)
	if err != nil {
		return err
	}

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}

	var dr dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		dr = dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	} else {
		dr = dyn.Resource(mapping.Resource)
	}

	_, err = dr.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: "sample-controller"})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Printf("resource %s already exists, continuing...", obj.GetName())
			return nil
		}
		return err
	}
	return nil
}

func deleteRayCluster(ctx context.Context, cfg *rest.Config, yamlContent []byte) error {
	dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	obj := &unstructured.Unstructured{}
	_, gvk, err := dec.Decode(yamlContent, nil, obj)
	if err != nil {
		return err
	}

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}

	var dr dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		dr = dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	} else {
		dr = dyn.Resource(mapping.Resource)
	}

	err = dr.Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			log.Printf("resource %s not found, continuing...", obj.GetName())
			return nil
		}
		return err
	}
	return nil
}
