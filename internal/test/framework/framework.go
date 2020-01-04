package framework

import (
	"fmt"
	"log"
	"os"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	// kubeconfig is the location of the kubeconfig for the cluster the test suite will run on
	kubeconfig string
	// awsCredentials is the Credentials file for the aws account the cluster will be created with
	awsCredentials string
	// artifactDir is the directory CI will read from once the test suite has finished execution
	artifactDir string
	// privateKeyPath is the path to the key that will be used to retrieve the password of each Windows VM
	privateKeyPath string
)

// TestFramework holds the info to run the test suite.
type TestFramework struct {
	// WinVms contains the Windows VMs that are created to execute the test suite
	WinVMs []WindowsVM
	// k8sclientset is the kubernetes clientset we will use to query the cluster's status
	K8sclientset *kubernetes.Clientset
	// OSConfigClient is the OpenShift config client, we will use to query the OpenShift api object status
	OSConfigClient *configclient.Clientset
}

// initCIvars gathers the values of the environment variables which configure the test suite
func initCIvars() error {
	kubeconfig = os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		return fmt.Errorf("KUBECONFIG environment variable not set")
	}
	awsCredentials = os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if awsCredentials == "" {
		return fmt.Errorf("AWS_SHARED_CREDENTIALS_FILE environment variable not set")
	}
	artifactDir = os.Getenv("ARTIFACT_DIR")
	if awsCredentials == "" {
		return fmt.Errorf("ARTIFACT_DIR environment variable not set")
	}
	privateKeyPath = os.Getenv("KUBE_SSH_KEY_PATH")
	if privateKeyPath == "" {
		return fmt.Errorf("KUBE_SSH_KEY_PATH environment variable not set")
	}
	return nil
}

// Setup creates and initializes a variable amount of Windows VMs
func (f *TestFramework) Setup(vmCount int) error {
	// Use Windows 2019 server image with containers in us-east1 zone for CI testing.
	// TODO: Move to environment variable that can be fetched from the cloud provider
	// The CI-operator uses AWS region `us-east-1` which has the corresponding image ID: ami-0b8d82dea356226d3 for
	// Microsoft Windows Server 2019 Base with Containers.
	imageID := "ami-0b8d82dea356226d3"
	// Using an AMD instance type, as the Windows hybrid overlay currently does not work on on machines using
	// the Intel 82599 network driver
	instanceType := "m5a.large"
	if err := initCIvars(); err != nil {
		return fmt.Errorf("unable to initialize CI variables: %v", err)
	}
	f.WinVMs = make([]WindowsVM, vmCount)
	// TODO: make them run in parallel: https://issues.redhat.com/browse/WINC-178
	for i := 0; i < vmCount; i++ {
		var err error
		f.WinVMs[i], err = newWindowsVM(imageID, instanceType)
		if err != nil {
			return fmt.Errorf("unable to instantiate Windows VM: %v", err)
		}
	}
	if err := f.getKubeClient(); err != nil {
		return fmt.Errorf("unable to get kube client: %v", err)
	}
	if err := f.getOpenShiftConfigClient(); err != nil {
		return fmt.Errorf("unable to get OpenShift client: %v", err)
	}
	return nil
}

// getKubeClient setups the kubeclient that can be used across all the test suites.
func (f *TestFramework) getKubeClient() error {
	kubeconfig := os.Getenv("KUBECONFIG")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("could not build config from flags: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("could not create k8s clientset: %v", err)
	}
	f.K8sclientset = clientset
	return nil
}

// getOpenShiftConfigClient gets the new OpenShift config v1 client
func (f *TestFramework) getOpenShiftConfigClient() error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("could not build config from flags: %v", err)
	}
	// Get openshift api config client.
	configClient, err := configclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("could not create config clientset: %v", err)
	}
	f.OSConfigClient = configClient
	return nil
}

// TearDown destroys the resources created by the Setup function
func (f *TestFramework) TearDown() {
	if f.WinVMs == nil {
		return
	}

	for _, vm := range f.WinVMs {
		if vm == nil {
			continue
		}
		if err := vm.Destroy(); err != nil {
			log.Printf("failed tearing down the Windows VM %v with error: %v", vm, err)
		} else {
			// WNI will delete all the VMs in windows-node-installer.json so we need this to succeed only once
			return
		}
	}
}
