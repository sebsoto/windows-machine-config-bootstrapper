package e2e

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/labels"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/masterzen/winrm"
	"github.com/openshift/windows-machine-config-operator/tools/windows-node-installer/pkg/cloudprovider"
	"github.com/openshift/windows-machine-config-operator/tools/windows-node-installer/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/api/certificates/v1beta1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	// Get kubeconfig, AWS credentials, and artifact dir from environment variable set by the OpenShift CI operator.
	kubeconfig     = os.Getenv("KUBECONFIG")
	awsCredentials = os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	dir            = os.Getenv("ARTIFACT_DIR")
	privateKeyPath = os.Getenv("KUBE_SSH_KEY_PATH")

	// Path of the WSU playbook
	playbookPath = os.Getenv("WSU_PATH")
	// clusterAddress is the address of the OpenShift cluster e.g. "foo.fah.com".
	// This should not include "https://api-" or a port
	clusterAddress = os.Getenv("CLUSTER_ADDR")

	// The CI-operator uses AWS region `us-east-1` which has the corresponding image ID: ami-0b8d82dea356226d3 for
	// Microsoft Windows Server 2019 Base with Containers.
	imageID = "ami-0b8d82dea356226d3"
	// Using an AMD instance type, as the Windows hybrid overlay currently does not work on on machines using
	// the Intel 82599 network driver
	instanceType = "m5a.large"
	sshKey       = "libra"

	// Cloud provider factory that we will use in these tests
	cloud cloudprovider.Cloud
	// Credentials for a spun up instance
	createdInstanceCreds *types.Credentials
	// Temp directory ansible created on the windows host
	ansibleTempDir = ""
	// kubernetes-node-windows-amd64.tar.gz SHA512
	// Value from https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG-1.16.md#node-binaries-1
	// This value should be updated when we change the kubelet version in WSU
	expectedKubeTarSha = "a88e7a1c6f72ea6073dbb4ddfe2e7c8bd37c9a56d94a33823f531e303a9915e7a844ac5880097724e44dfa7f4" +
		"a9659d14b79cc46e2067f6b13e6df3f3f1b0f64"
	// k8sclientset is the kubernetes clientset we will use to query the cluster's status
	k8sclientset *kubernetes.Clientset
	// winRMClient is the WinRM client we can use to remotely run commands on the Windows node
	winRMClient *winrm.Client
	// workerLabel is the worker label that needs to be applied to the Windows node
	workerLabel = "node-role.kubernetes.io/worker"
	// windowsLabel represents the node label that need to be applied to the Windows node created
	windowsLabel = "node.openshift.io/os_id=Windows"
	// hybridOverlaySubnet is an annotation applied by the cluster network operator which is used by the hybrid overlay
	hybridOverlaySubnet = "k8s.ovn.org/hybrid-overlay-hostsubnet"
	// hybridOverlayMac is an annotation applied by the hybrid overlay
	hybridOverlayMac = "k8s.ovn.org/hybrid-overlay-distributed-router-gateway-mac"

	// windowsServerImage is the name/location of the Windows Server 2019 image we will use to test pod deployment
	windowsServerImage = "mcr.microsoft.com/windows/servercore:ltsc2019"
	// ubi8Image is the name/location of the linux image we will use for testing
	ubi8Image = "registry.access.redhat.com/ubi8/ubi:latest"
)

// createAWSWindowsInstance creates a windows instance and populates the "cloud" and "createdInstanceCreds" global
// variables
func createAWSWindowsInstance() error {
	var err error
	cloud, err = cloudprovider.CloudProviderFactory(kubeconfig, awsCredentials, "default", dir,
		imageID, instanceType, sshKey, privateKeyPath)
	if err != nil {
		return fmt.Errorf("could not setup cloud provider: %s", err)
	}
	createdInstanceCreds, err = cloud.CreateWindowsVM()
	if err != nil {
		return fmt.Errorf("could not create windows VM: %s", err)
	}
	return nil
}

// createhostFile creates an ansible host file and returns the path of it
func createHostFile(ip, password string) (string, error) {
	hostFile, err := ioutil.TempFile("", "testWSU")
	if err != nil {
		return "", fmt.Errorf("coud not make temporary file: %s", err)
	}
	defer hostFile.Close()

	_, err = hostFile.WriteString(fmt.Sprintf(`[win]
%s ansible_password='%s'

[win:vars]
ansible_user=Administrator
cluster_address=%s
ansible_port=5986
ansible_connection=winrm
ansible_winrm_server_cert_validation=ignore`, ip, password, clusterAddress))
	return hostFile.Name(), err
}

// getKubeClient returns a pointer to a kubernetes clientset given the path to a cluster's kubeconfig
func getKubeClient(kubeconfig string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("could not build config from flags: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("could not create k8s clientset: %v", err)
	}
	return clientset, nil
}

// TestWSU creates a Windows instance, runs the WSU, and then runs a series of tests to ensure all expected
// behavior was achieved. The following environment variables must be set for this test to run: KUBECONFIG,
// AWS_SHARED_CREDENTIALS_FILE, ARTIFACT_DIR, KUBE_SSH_KEY_PATH, WSU_PATH, CLUSTER_ADDR
func TestWSU(t *testing.T) {
	require.NotEmptyf(t, kubeconfig, "KUBECONFIG environment variable not set")
	require.NotEmptyf(t, awsCredentials, "AWS_SHARED_CREDENTIALS_FILE environment variable not set")
	require.NotEmptyf(t, dir, "ARTIFACT_DIR environment variable not set")
	require.NotEmptyf(t, privateKeyPath, "KUBE_SSH_KEY_PATH environment variable not set")
	require.NotEmptyf(t, playbookPath, "WSU_PATH environment variable not set")
	require.NotEmptyf(t, clusterAddress, "CLUSTER_ADDR environment variable not set")

	var err error
	k8sclientset, err = getKubeClient(kubeconfig)
	require.NoError(t, err)
	existingWindowsNodes, err := k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: windowsLabel})
	require.NoError(t, err)
	// The test should fail, if there are existing Windows nodes in the cluster as we're just relying on the
	// windowsLabel to be present on the node
	// TODO: Label or annotate the windows node to have more determinism when running test suite.
	require.Equalf(t, len(existingWindowsNodes.Items), 0, "expected 0 windows nodes to be present but found %v",
		len(existingWindowsNodes.Items))

	// TODO: Check if other cloud provider credentials are available
	if awsCredentials == "" {
		t.Fatal("No cloud provider credentials available")
	}
	err = createAWSWindowsInstance()
	require.NoErrorf(t, err, "Error spinning up Windows VM: %s", err)
	require.NotNil(t, createdInstanceCreds, "Instance credentials are not set")
	defer cloud.DestroyWindowsVMs()
	// In order to run the ansible playbook we create an inventory file:
	// https://docs.ansible.com/ansible/latest/user_guide/intro_inventory.html
	hostFilePath, err := createHostFile(createdInstanceCreds.GetIPAddress(), createdInstanceCreds.GetPassword())
	require.NoErrorf(t, err, "Could not write to host file: %s", err)
	cmd := exec.Command("ansible-playbook", "-vvv", "-i", hostFilePath, playbookPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "WSU playbook returned error: %s, with output: %s", err, string(out))

	// Ansible will copy files to a temporary directory with a path such as:
	// C:\\Users\\Administrator\\AppData\\Local\\Temp\\ansible.z5wa1pc5.vhn\\
	initialSplit := strings.Split(string(out), "C:\\\\Users\\\\Administrator\\\\AppData\\\\Local\\\\Temp\\\\ansible.")
	require.True(t, len(initialSplit) > 1, "Could not find Windows temp dir: %s", out)
	ansibleTempDir = "C:\\Users\\Administrator\\AppData\\Local\\Temp\\ansible." + strings.Split(initialSplit[1], "\"")[0]

	// Connect to the bootstrapped host. Timeout is high as the Windows Server image is slow to download
	endpoint := winrm.NewEndpoint(createdInstanceCreds.GetIPAddress(), 5986, true, true,
		nil, nil, nil, time.Minute*10)
	winRMClient, err = winrm.NewClient(endpoint, "Administrator", createdInstanceCreds.GetPassword())
	require.NoErrorf(t, err, "Could not create winrm client: %s", err)

	t.Run("Files copied to Windows node", testFilesCopied)
	t.Run("Pending CSRs were approved", testNoPendingCSRs)
	t.Run("Node is in ready state", testNodeReady)
	// test if the Windows node has proper worker label.
	t.Run("Check if worker label has been applied to the Windows node", testWorkerLabelsArePresent)
	t.Run("Network annotations were applied to node", testHybridOverlayAnnotations)
	t.Run("HNS Networks were created", testHNSNetworksCreated)
	t.Run("East-west networking", testEastWestNetworking)
}

// testFilesCopied tests that the files we attempted to copy to the Windows host, exist on the Windows host
func testFilesCopied(t *testing.T) {
	expectedFileList := []string{"kubelet.exe", "worker.ign", "wmcb.exe", "hybrid-overlay.exe", "kube.tar.gz"}

	// Check if each of the files we expect on the Windows host are there
	for _, filename := range expectedFileList {
		fullPath := ansibleTempDir + "\\" + filename
		// This command will write to stdout, only if the file we are looking for does not exist
		command := fmt.Sprintf("if not exist %s echo fail", fullPath)
		stdout := new(bytes.Buffer)
		_, err := winRMClient.Run(command, stdout, os.Stderr)
		assert.NoError(t, err, "Error looking for %s: %s", fullPath, err)
		assert.Emptyf(t, stdout.String(), "Missing file: %s", fullPath)
	}

	// Check the SHA of kube.tar.gz downloaded
	kubeTarPath := ansibleTempDir + "\\" + "kube.tar.gz"
	// certutil is part of default OS installation Windows 7+
	command := fmt.Sprintf("certutil -hashfile %s SHA512", kubeTarPath)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	_, err := winRMClient.Run(command, stdout, stderr)
	require.NoError(t, err, "Error generating SHA512 for %s", kubeTarPath)
	require.Equalf(t, stderr.Len(), 0, "Error generating SHA512 for %s", kubeTarPath)
	// CertUtil output example:
	// SHA512 hash of <filepath>:\r\n<SHA-output>\r\nCertUtil: -hashfile command completed successfully.
	// Extracting SHA value from the output
	actualKubeTarSha := strings.Split(stdout.String(), "\r\n")[1]
	assert.Equal(t, expectedKubeTarSha, actualKubeTarSha,
		"kube.tar.gz downloaded does not match expected checksum")
}

// testNodeReady tests that the bootstrapped node was added to the cluster and is in the ready state
func testNodeReady(t *testing.T) {
	var createdNode *v1.Node
	nodes, err := k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{})
	require.NoError(t, err, "Could not get list of nodes")
	require.NotZero(t, len(nodes.Items), "No nodes found")

	// Find the node that we spun up
	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if address.Type == "ExternalIP" && address.Address == createdInstanceCreds.GetIPAddress() {
				createdNode = &node
				break
			}
		}
		if createdNode != nil {
			break
		}
	}
	require.NotNil(t, createdNode, "Created node not found through Kubernetes API")

	// Make sure the node is in a ready state
	foundReady := false
	for _, condition := range createdNode.Status.Conditions {
		if condition.Type != v1.NodeReady {
			continue
		}
		foundReady = true
		assert.Equalf(t, v1.ConditionTrue, condition.Status, "Node not in ready state: %s", condition.Reason)
		break
	}
	// Just in case node is missing the ready condition, for whatever reason
	assert.True(t, foundReady, "Node did not have ready condition")
}

// testNoPendingCSRs tests that there are no pending CSRs on the cluster
func testNoPendingCSRs(t *testing.T) {
	csrs, err := k8sclientset.CertificatesV1beta1().CertificateSigningRequests().List(metav1.ListOptions{})
	assert.NoError(t, err, "could not get CSR list")
	for _, csr := range csrs.Items {
		// CSR's with an empty condition list are pending
		assert.NotEmptyf(t, csr.Status.Conditions, "csr %v is pending", csr)
		// If not pending, make sure the CSR is approved
		for _, condition := range csr.Status.Conditions {
			assert.Equalf(t, v1beta1.CertificateApproved, condition.Type, "csr %v has non-approved condition", csr)
		}
	}
}

// testWorkerLabelsArePresent tests if the worker labels are present on the Windows Node.
func testWorkerLabelsArePresent(t *testing.T) {
	// Check if the Windows node has the required label needed.
	windowsNodes, err := k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: windowsLabel})
	require.NoErrorf(t, err, "error while getting Windows node: %v", err)
	assert.Equalf(t, len(windowsNodes.Items), 1, "expected 1 windows nodes to be present but found %v",
		len(windowsNodes.Items))
	assert.Contains(t, windowsNodes.Items[0].Labels, workerLabel,
		"expected worker label to be present on the Windows node but did not find any")
}

// testHybridOverlayAnnotations tests that the correct annotations have been added to the bootstrapped node
func testHybridOverlayAnnotations(t *testing.T) {
	windowsNodes, err := k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: windowsLabel})
	require.NoError(t, err, "Could not get list of Windows nodes")
	assert.Equalf(t, len(windowsNodes.Items), 1, "expected one windows node to be present but found %v",
		len(windowsNodes.Items))
	assert.Contains(t, windowsNodes.Items[0].Annotations, hybridOverlaySubnet)
	assert.Contains(t, windowsNodes.Items[0].Annotations, hybridOverlayMac)
}

// testHNSNetworksCreated tests that the required HNS Networks have been created on the bootstrapped node
func testHNSNetworksCreated(t *testing.T) {
	stdout := new(bytes.Buffer)
	_, err := winRMClient.Run("powershell Get-HnsNetwork", stdout, os.Stderr)
	require.NoError(t, err, "Could not run Get-HnsNetwork command")
	stdoutString := stdout.String()
	assert.Contains(t, stdoutString, "Name                   : BaseOpenShiftNetwork",
		"Could not find BaseOpenShiftNetwork in list of HNS Networks")
	assert.Contains(t, stdoutString, "Name                   : OpenShiftNetwork",
		"Could not find OpenShiftNetwork in list of HNS Networks")
}

// testEastWestNetworking deploys Windows and Linux pods, and tests that the pods can communicate
func testEastWestNetworking(t *testing.T) {
	// Preload the image that will be used on the Windows node, to prevent download timeouts
	// and separate possible failure conditions into multiple operations
	err := pullDockerImage(windowsServerImage)
	require.NoError(t, err, "Could not pull Windows Server image")

	// This will run a Server on the container, which can be reached with a GET request
	winServerCommand := []string{"powershell.exe", "-command",
		"<#code used from https://gist.github.com/wagnerandrade/5424431#> ; $$listener = " +
			"New-Object System.Net.HttpListener ; $$listener.Prefixes.Add('http://*:80/') ; " +
			"$$listener.Start() ; $$callerCounts = @{} ; Write-Host('Listening at http://*:80/')" +
			" ; while ($$listener.IsListening) { ;$$context = $$listener.GetContext() ;" +
			"$$requestUrl = $$context.Request.Url ;$$clientIP = " +
			"$$context.Request.RemoteEndPoint.Address ;$$response = $$context.Response ;" +
			"Write-Host '' ;Write-Host('> {0}' -f $$requestUrl) ;  ;$$count = 1 ;" +
			"$$k=$$callerCounts.Get_Item($$clientIP) ;if ($$k -ne $$null) { $$count += $$k } ;" +
			"$$callerCounts.Set_Item($$clientIP, $$count) ;$$ip=(Get-NetAdapter | " +
			"Get-NetIpAddress); $$header='<html><body><H1>Windows Container Web Server</H1>' " +
			";$$callerCountsString='' ;$$callerCounts.Keys | % { $$callerCountsString+='<p>IP {0}" +
			" callerCount {1} ' -f $$ip[1].IPAddress,$$callerCounts.Item($$_) } " +
			";$$footer='</body></html>' ;$$content='{0}{1}{2}' -f " +
			"$$header,$$callerCountsString,$$footer ;Write-Output $$content ;$$buffer = " +
			"[System.Text.Encoding]::UTF8.GetBytes($$content) ;$$response.ContentLength64 = " +
			"$$buffer.Length ;$$response.OutputStream.Write($$buffer, 0, $$buffer.Length) ;" +
			"$$response.Close() ;$$responseStatus = $$response.StatusCode ;" +
			"Write-Host('< {0}' -f $$responseStatus)  } ; "}
	winServerDeployment, err := createWindowsServerDeployment("win-server", winServerCommand)
	require.NoError(t, err, "Could not create Windows deployment")
	defer deleteDeployment(winServerDeployment.Name)
	// Wait until the server is ready to be queried
	winServerPod, err := waitUntilPodHasRunningContainer(*winServerDeployment.Spec.Selector)
	require.NoError(t, err, "Could not retrieve pod with selector %v", *winServerDeployment.Spec.Selector)

	// test Windows <-> Linux
	// This will install curl and then curl the windows server.
	linuxCurlerCommand := []string{"bash", "-c", "yum update; yum install curl -y; curl " + winServerPod.Status.PodIP}
	linuxCurlerJob, err := createLinuxJob("linux-curler", linuxCurlerCommand)
	require.NoError(t, err, "Could not create Linux job")
	defer deleteJob(linuxCurlerJob.Name)
	err = waitUntilJobSucceeds(linuxCurlerJob.Name)
	assert.NoError(t, err, "Could not curl the Windows server from a linux container")

	// test Windows <-> Windows on same node
	// This will continually try to read from the Windows Server. We have to try multiple times as the network is flaky
	// on container startup
	winCurlerCommand := []string{"powershell.exe", "-command", "for (($i =0), ($j = 0); $i -lt 10; $i++) { " +
		"$response = Invoke-Webrequest -UseBasicParsing -Uri " + winServerPod.Status.PodIP +
		"; $code = $response.StatusCode; echo \"GET returned code $code\";" +
		"If ($code -eq 200) {exit 0}; Start-Sleep -s 10;}; exit 1" + winServerPod.Status.PodIP}
	winCurlerJob, err := createWindowsServerJob("win-curler", winCurlerCommand)
	require.NoError(t, err, "Could not create Windows job")
	defer deleteJob(winCurlerJob.Name)
	err = waitUntilJobSucceeds(winCurlerJob.Name)
	assert.NoError(t, err, "Could not curl the Windows server from a Windows container")

	// TODO: test Windows <-> Windows on different node
}

// waitUntilJobSucceeds will return an error if the job fails or reaches a timeout
func waitUntilJobSucceeds(name string) error {
	var job *batchv1.Job
	var err error
	for i := 0; i < 10; i++ {
		job, err = k8sclientset.BatchV1().Jobs(v1.NamespaceDefault).Get(name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if job.Status.Succeeded > 0 {
			return nil
		}
		if job.Status.Failed > 0 {
			return fmt.Errorf("job %v failed", job)
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("job %v timed out", job)
}

// waitUntilPodHasRunningContainer will return a pod that matches the selector, once the container specified by the pod
// is in the "Running" state
func waitUntilPodHasRunningContainer(selector metav1.LabelSelector) (v1.Pod, error) {
	var err error
	pod := v1.Pod{}
	for i := 0; i < 10; i++ {
		pod, err = getPodWithLabelSelector(selector)
		if err != nil {
			return v1.Pod{}, err
		}
		if len(pod.Status.ContainerStatuses) > 1 {
			return v1.Pod{}, fmt.Errorf("expected one container within the pod but found %d",
				len(pod.Status.ContainerStatuses))
		} else if len(pod.Status.ContainerStatuses) == 1 {
			container := pod.Status.ContainerStatuses[0]
			if container.State.Running != nil {
				return pod, nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	return pod, fmt.Errorf("timed out waiting for pod %v to have ready container", pod)
}

// getPodWithLabelSelector returns the pod that matches the label selector. If more than one pod match the selector,
// the function will return an error
func getPodWithLabelSelector(selector metav1.LabelSelector) (v1.Pod, error) {
	selectorString := labels.Set(selector.MatchLabels).String()
	podList, err := k8sclientset.CoreV1().Pods(v1.NamespaceDefault).List(metav1.ListOptions{LabelSelector: selectorString})
	if err != nil {
		return v1.Pod{}, err
	}
	if len(podList.Items) != 1 {
		return v1.Pod{}, fmt.Errorf("expected one pod matching %s, but found %d", selectorString, len(podList.Items))
	}

	return podList.Items[0], nil
}

// createWindowsServerJob creates a job which will run the provided command with a Windows Server image
func createWindowsServerJob(name string, command []string) (*batchv1.Job, error) {
	windowsNodeSelector := map[string]string{"beta.kubernetes.io/os": "windows"}
	windowsTolerations := []v1.Toleration{{Key: "os", Value: "Windows", Effect: v1.TaintEffectNoSchedule}}
	return createJob(name, windowsServerImage, command, windowsNodeSelector, windowsTolerations)
}

// createLinuxJob creates a job which will run the provided command with a ubi8 image
func createLinuxJob(name string, command []string) (*batchv1.Job, error) {
	return createJob(name, ubi8Image, command, map[string]string{}, []v1.Toleration{})
}

func createJob(name, image string, command []string, selector map[string]string, tolerations []v1.Toleration) (*batchv1.Job, error) {
	jobsClient := k8sclientset.BatchV1().Jobs(v1.NamespaceDefault)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-job",
		},
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					Tolerations:   tolerations,
					Containers: []v1.Container{
						{
							Name:            name,
							Image:           image,
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         command,
						},
					},
					NodeSelector: selector,
				},
			},
		},
	}

	// Create job
	job, err := jobsClient.Create(job)
	if err != nil {
		return nil, err
	}
	return job, err
}

// deleteJob deletes the job with the given name
func deleteJob(name string) error {
	jobsClient := k8sclientset.BatchV1().Jobs(v1.NamespaceDefault)
	return jobsClient.Delete(name, &metav1.DeleteOptions{})
}

// createWindowsServerDeployment creates a deployment with a Windows Server 2019 container
func createWindowsServerDeployment(name string, command []string) (*appsv1.Deployment, error) {
	deploymentsClient := k8sclientset.AppsV1().Deployments(v1.NamespaceDefault)
	replicaCount := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-deployment",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicaCount,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": name,
					},
				},
				Spec: v1.PodSpec{
					Tolerations: []v1.Toleration{
						{
							Key:    "os",
							Value:  "Windows",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					Containers: []v1.Container{
						// Windows web server
						{
							Name:            name,
							Image:           windowsServerImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         command,
						},
					},
					NodeSelector: map[string]string{"beta.kubernetes.io/os": "windows"},
				},
			},
		},
	}

	// Create Deployment
	deploy, err := deploymentsClient.Create(deployment)
	if err != nil {
		return nil, err
	}
	return deploy, err
}

// deleteDeployment deletes the deployment with the given name
func deleteDeployment(name string) error {
	deploymentsClient := k8sclientset.AppsV1().Deployments(v1.NamespaceDefault)
	return deploymentsClient.Delete(name, &metav1.DeleteOptions{})
}

// pullDockerImage pulls the designated image on the remote host
func pullDockerImage(name string) error {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	command := "docker pull " + name
	errorCode, err := winRMClient.Run(command, stdout, stderr)
	if err != nil {
		return fmt.Errorf("failed to remotely run docker pull: %s", err)
	}
	stderrString := stdout.String()
	if errorCode != 0 {
		return fmt.Errorf("return code %d, stderr: %s", errorCode, stderrString)
	}
	return nil
}
