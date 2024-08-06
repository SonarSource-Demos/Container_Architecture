package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/briandowns/spinner"
	"github.com/golang/glog"
	_ "github.com/lib/pq"
	yaml1 "gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

type Configuration struct {
	ClusterName  string
	NSDataBase   string
	PvcDBsize    string
	PGSecret     string
	StorageClass string
	Sonaruser    string
	Sonarpass    string
	PGsql        string
	PGconf       string
	PGsvc        string
}

func GetConfig(configjs Configuration) Configuration {

	fconfig, err := os.ReadFile("config.json")
	if err != nil {
		panic("❌ Problem with the configuration file : config.json")
		os.Exit(1)
	}
	if err := json.Unmarshal(fconfig, &configjs); err != nil {
		fmt.Println("❌ Error unmarshaling JSON:", err)
		os.Exit(1)
	}

	return configjs
}

func applyResourcesFromYAML(yamlContent []byte, clientset *kubernetes.Clientset, dd *dynamic.DynamicClient, ns string) error {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(yamlContent), 100)

	for {
		var rawObj runtime.RawExtension
		if err := decoder.Decode(&rawObj); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(rawObj.Raw, nil, nil)
		if err != nil {
			return err
		}
		unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return err
		}
		unstructuredObj := &unstructured.Unstructured{Object: unstructuredMap}
		gr, err := restmapper.GetAPIGroupResources(clientset.Discovery())
		if err != nil {
			return err
		}
		mapper := restmapper.NewDiscoveryRESTMapper(gr)
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return err
		}
		var dri dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if unstructuredObj.GetNamespace() == "" {
				unstructuredObj.SetNamespace(ns)
			}
			dri = dd.Resource(mapping.Resource).Namespace(unstructuredObj.GetNamespace())
		} else {
			dri = dd.Resource(mapping.Resource)
		}
		_, err = dri.Create(context.Background(), unstructuredObj, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func waitForServiceReady(clientset *kubernetes.Clientset, serviceName, namespace string, pollingInterval time.Duration) (string, string, error) {
	for {
		service, err := clientset.CoreV1().Services(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
		if err != nil {
			return "", "", err
		}

		if len(service.Status.LoadBalancer.Ingress) > 0 {
			externalIP := service.Status.LoadBalancer.Ingress[0].Hostname
			if externalIP != "" {
				clusterIP := service.Spec.ClusterIP
				return externalIP, clusterIP, nil
			}
		}

		time.Sleep(pollingInterval)
	}
}

func deleteNamespace(clientset *kubernetes.Clientset, namespace string) error {
	// Delete the namespace.
	err := clientset.CoreV1().Namespaces().Delete(context.TODO(), namespace, metav1.DeleteOptions{})
	if err != nil {
		return err
	}

	// Wait for the namespace to be deleted.
	for {
		_, err := clientset.CoreV1().Namespaces().Get(context.TODO(), namespace, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				fmt.Printf("\n✅ Namespace %s has been deleted\n", namespace)
				break
			}
		}
		time.Sleep(2 * time.Second)
	}

	return nil
}

// LoadConfigFromFile loads YAML configuration from a file
func LoadConfigFromFile(filePath string, config interface{}) (interface{}, error) {
	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	err = yaml1.Unmarshal(fileContent, config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func main() {

	var config1 Configuration
	var AppConfig = GetConfig(config1)

	pollingInterval := 5 * time.Second

	configMapData := make(map[string]string, 0)
	initdb := `
	psql -v ON_ERROR_STOP=1 --username "postgres" --dbname "postgres" <<-EOSQL
	CREATE ROLE ` + AppConfig.Sonaruser + ` WITH LOGIN PASSWORD '` + AppConfig.Sonarpass + `';
	CREATE DATABASE sonarqube WITH ENCODING 'UTF8' OWNER ` + AppConfig.Sonaruser + ` TEMPLATE=template0;
	GRANT ALL PRIVILEGES ON DATABASE sonarqube TO ` + AppConfig.Sonaruser + `;
	EOSQL
	`
	configMapData["init.sh"] = initdb

	// Parse command-line arguments
	cmdArgs := os.Args[1:]

	// Load Kubeconfig
	kubeconfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("❌ Failed to create a ClientSet: %v. Exiting.", err)
	}

	if len(cmdArgs) != 1 || (cmdArgs[0] != "deploy" && cmdArgs[0] != "destroy") {
		fmt.Println("❌ Usage: go run main.go [deploy|destroy]")
		os.Exit(1)
	}

	/*------------------------- Main -----------------------------*/

	if cmdArgs[0] == "deploy" {

		spin := spinner.New(spinner.CharSets[9], 100*time.Millisecond)
		spin.Prefix = "Deployment PostgreSQL Database : "
		spin.Color("green", "bold")
		spin.Start()

		fmt.Printf("\r%s %s \n", spin.Prefix, "Creating namespace...")
		// Create a Namespace Database
		nsName := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: AppConfig.NSDataBase,
			},
		}
		_, err = clientset.CoreV1().Namespaces().Create(context.Background(), nsName, metav1.CreateOptions{})
		if err != nil {
			spin.Stop()
			fmt.Printf("❌ Error creating namespace: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\r✅ Namespace %s created successfully\n", AppConfig.NSDataBase)

		fmt.Printf("\r%s %s \n", spin.Prefix, "Creating PVC...")

		// Create a PVC for database
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pgsql-data",
				Namespace: AppConfig.NSDataBase,
			},
			Spec: v1.PersistentVolumeClaimSpec{
				AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
				StorageClassName: &AppConfig.StorageClass,
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceStorage: resource.MustParse(AppConfig.PvcDBsize),
					},
				},
			},
		}
		_, err := clientset.CoreV1().PersistentVolumeClaims(AppConfig.NSDataBase).Create(context.TODO(), pvc, metav1.CreateOptions{})
		if err != nil {
			spin.Stop()
			fmt.Printf("\n❌ Error creating PVC: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("\r✅ PVC Database : pgsql-data created successfully\n")

		fmt.Printf("\r%s %s \n", spin.Prefix, "Creating secret database...")

		//Create a secret database
		dd, err := dynamic.NewForConfig(config)
		if err != nil {
			log.Fatal(err)
		}

		pvYAML, err := os.ReadFile(AppConfig.PGSecret)
		if err != nil {
			spin.Stop()
			fmt.Printf("\n ❌ Error reading Secret YAML file %s: %v\n", err, AppConfig.PGSecret)
			os.Exit(1)
		}
		err = applyResourcesFromYAML(pvYAML, clientset, dd, AppConfig.NSDataBase)
		if err != nil {
			spin.Stop()
			log.Fatalf("\n ❌ Error applying %s file %v\n", err, AppConfig.PGSecret)
			return
		}
		fmt.Println("\r✅ Database secret created successfully\n")

		fmt.Printf("\r%s %s \n", spin.Prefix, "Creating ConfigMap Init DB...")

		// Create a ConfigMap Init DB
		PGsqlInit := v1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				Kind:       "ConfigMap",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pgsql-init",
				Namespace: AppConfig.NSDataBase,
			},
			Data: configMapData,
		}
		_, err1 := clientset.CoreV1().ConfigMaps(AppConfig.NSDataBase).Create(context.TODO(), &PGsqlInit, metav1.CreateOptions{})
		if err1 != nil {
			spin.Stop()
			fmt.Printf("\n ❌ Error creating PGSQLInit configMaps: %v\n", err1)
			os.Exit(1)
		}
		fmt.Println("\r✅ PGSQLInit configMaps created successfully\n")

		fmt.Printf("\r%s %s \n", spin.Prefix, "Creating ConfigMap DATA DB...")
		// Create a ConfigMap DATA DB
		pgcYAML, err := os.ReadFile(AppConfig.PGconf)
		if err != nil {
			spin.Stop()
			fmt.Printf("\n ❌ Error reading Secret YAML file %s: %v\n", err, AppConfig.PGconf)
			os.Exit(1)
		}
		err = applyResourcesFromYAML(pgcYAML, clientset, dd, AppConfig.NSDataBase)
		if err != nil {
			spin.Stop()
			log.Fatalf("\n ❌ Error applying %s file %v\n", err, AppConfig.PGconf)
			return
		}

		fmt.Println("\r✅ PGSQLData configMaps created successfully\n")

		fmt.Printf("\r%s %s \n", spin.Prefix, "Deploy Postgresql deployment...")

		// Deploy Postgresql

		pgYAML, err := os.ReadFile(AppConfig.PGsql)
		if err != nil {
			spin.Stop()
			fmt.Printf("\n ❌ Error reading PGSQL YAML file %s: %v\n", err, AppConfig.PGsql)
			os.Exit(1)
		}
		err = applyResourcesFromYAML(pgYAML, clientset, dd, AppConfig.NSDataBase)
		if err != nil {
			spin.Stop()
			log.Fatalf("\n ❌ Error applying %s file %v\n", err, AppConfig.PGsql)
			return
		}

		externalIP, ClusterIP, err := waitForServiceReady(clientset, AppConfig.PGsvc, AppConfig.NSDataBase, pollingInterval)
		if err != nil {
			spin.Stop()
			fmt.Printf("\n ❌ Error waiting for service to become ready: %v\n", err)
			os.Exit(1)
		}

		JDBCURL := "jdbc:postgresql://" + AppConfig.PGsvc + "." + AppConfig.NSDataBase + ".svc.cluster.local:5432/sonarqube?currentSchema=public"
		//JDBCURL := "jdbc:postgresql://" + externalIP + ":5432/sonarqube?currentSchema=public"
		spin.Stop()
		fmt.Printf("\n✅ PostgreSQL Database Successful deployment External IP: %s\n", externalIP)
		fmt.Printf("✅ JDBC URL : %s - IP : %s\n\n\n", JDBCURL, ClusterIP)

	} else if cmdArgs[0] == "destroy" {

		/*--------------------------------- Destroy Steps ------------------------------------*/

		spin := spinner.New(spinner.CharSets[9], 100*time.Millisecond)
		spin.Color("green", "bold")
		spin.Start()

		spin.Suffix = "Destroy Deployment Database : "
		spin.Start()

		fmt.Printf("\r%s %s \n", spin.Prefix, "Destroy the Database Namespace...")
		// Destroy the Database Namespace
		if err := deleteNamespace(clientset, AppConfig.NSDataBase); err != nil {
			spin.Stop()
			fmt.Printf("\n❌ Error deleting namespace %s: %v\n", AppConfig.NSDataBase, err)
			os.Exit(1)
		}
		fmt.Println("\n ✅ Deployment Database deleted successfully\n")
		spin.Stop()

	}

}
