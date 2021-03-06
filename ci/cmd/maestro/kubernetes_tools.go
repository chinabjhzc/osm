package maestro

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/go-autorest/autorest/to"
	mapset "github.com/deckarep/golang-set"
	"k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// We are going to wait for the Pod certain amount of time if it is in one of these statuses
// See: https://github.com/kubernetes/kubernetes/blob/d0183703cbe715c879cb42db375c7373b7f2b6a1/pkg/kubelet/kubelet_test.go#L1453-L1454
var statusWorthWaitingFor = mapset.NewSet("ContainerCreating", "PodInitializing")

// GetPodLogs returns pod logs.
func GetPodLogs(kubeClient kubernetes.Interface, namespace string, podName string, containerName string, timeSince time.Duration) string {
	sinceTime := metav1.NewTime(time.Now().Add(-timeSince))
	options := &corev1.PodLogOptions{
		Container: containerName,
		Follow:    false,
		SinceTime: &sinceTime,
	}

	logStream, err := kubeClient.CoreV1().Pods(namespace).GetLogs(podName, options).Stream(context.Background())
	if err != nil {
		fmt.Println("Error in opening stream: ", err)
		os.Exit(1)
	}

	defer logStream.Close()
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(logStream)
	if err != nil {
		log.Error().Err(err).Msg("Error reading from pod logs stream")
	}
	return buf.String()
}

// DeleteNamespaces deletes the namespaces listed.
func DeleteNamespaces(client *kubernetes.Clientset, namespaces ...string) {
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: to.Int64Ptr(0),
	}

	for _, ns := range namespaces {
		if err := client.CoreV1().Namespaces().Delete(context.Background(), ns, deleteOptions); err != nil {
			log.Error().Err(err).Msgf("Error deleting namespace %s", ns)
			continue
		}
		log.Info().Msgf("Deleted namespace: %s", ns)
	}
}

// DeleteWebhook deletes the webhook by name.
func DeleteWebhook(client *kubernetes.Clientset, webhookName string) {
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: to.Int64Ptr(0),
	}

	var webhooks *v1beta1.MutatingWebhookConfigurationList
	var err error
	webhooks, err = client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Error().Err(err).Msg("Error listing webhooks")
	}

	for _, webhook := range webhooks.Items {
		if webhook.Name == webhookName {
			if err := client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Delete(context.Background(), webhook.Name, deleteOptions); err != nil {
				log.Error().Err(err).Msgf("Error deleting webhook %s", webhook.Name)
			}
			log.Info().Msgf("Deleted mutating webhook: %s", webhook.Name)
		}
	}
}

// GetPodName returns the name of the pod for the given selector.
func GetPodName(kubeClient kubernetes.Interface, namespace, selector string) (string, error) {
	podList, err := kubeClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}

	if len(podList.Items) == 0 {
		log.Error().Msgf("Zero pods found for selector %s in namespace %s", selector, namespace)
		return "", errNoPodsFound
	}

	sort.SliceStable(podList.Items, func(i, j int) bool {
		p1 := podList.Items[i].CreationTimestamp.UnixNano()
		p2 := podList.Items[j].CreationTimestamp.UnixNano()
		return p1 > p2
	})

	return podList.Items[0].Name, nil
}

// SearchLogsForSuccess tails logs until success enum is found.
// The pod/container we are observing is responsible for sending the SUCCESS/FAIL token based on local heuristic.
func SearchLogsForSuccess(kubeClient kubernetes.Interface, namespace string, podName string, containerName string, totalWait time.Duration, result chan TestResult, successToken, failureToken string) {
	sinceTime := metav1.NewTime(time.Now().Add(-PollLogsFromTimeSince))
	options := &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
		SinceTime: &sinceTime,
	}

	logStream, err := kubeClient.CoreV1().Pods(namespace).GetLogs(podName, options).Stream(context.Background())
	if err != nil {
		fmt.Println("Error in opening stream: ", err)
		os.Exit(1)
	}

	// Poll for success
	startedWaiting := time.Now()

	go func() {
		defer close(result)
		defer logStream.Close()
		r := bufio.NewReader(logStream)
		for {

			line, err := r.ReadString('\n')

			switch {

			// Make sure we don't wait too long for success/failure
			case time.Since(startedWaiting) >= totalWait:
				result <- TestsTimedOut

			// If we detect EOF before success - this must have bene a filure
			case err == io.EOF:
				log.Error().Err(err).Msgf("EOF reading from pod %s/%s", namespace, podName)
				result <- TestsFailed
				return

			// Any other error fails the test
			case err != nil:
				log.Error().Err(err).Msgf("Error reading from pod %s/%s", namespace, podName)
				result <- TestsFailed
				return

			// Finally search for SUCCESS or FAILURE
			// The container itself has the heuristic on when to emit these.
			default:

				if strings.Contains(line, successToken) {
					log.Info().Msgf("[%s] Found %s", containerName, successToken)
					result <- TestsPassed
					return
				}

				if strings.Contains(line, failureToken) {
					log.Info().Msgf("[%s] Found %s", containerName, failureToken)
					result <- TestsFailed
					return
				}
			}
		}
	}()
}

// GetKubernetesClient returns a k8s client.
func GetKubernetesClient() *kubernetes.Clientset {
	var kubeConfig *rest.Config
	var err error
	kubeConfigFile := os.Getenv(KubeConfigEnvVar)
	if kubeConfigFile != "" {
		kubeConfig, err = clientcmd.BuildConfigFromFlags("", kubeConfigFile)
		if err != nil {
			fmt.Printf("Error fetching Kubernetes config. Ensure correctness of CLI argument 'kubeconfig=%s': %s", kubeConfigFile, err)
			os.Exit(1)
		}
	} else {
		// creates the in-cluster config
		kubeConfig, err = rest.InClusterConfig()
		if err != nil {
			fmt.Printf("Error generating Kubernetes config: %s", err)
			os.Exit(1)
		}
	}

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Println("error in getting access to K8S")
		os.Exit(1)
	}
	return clientset
}

// WaitForPodToBeReady waits for a pod by selector to be ready.
func WaitForPodToBeReady(kubeClient kubernetes.Interface, totalWait time.Duration, namespace, selector string, wg *sync.WaitGroup) {
	startedWaiting := time.Now()

	for {
		if time.Since(startedWaiting) >= totalWait {
			log.Error().Msgf("Waited for pod %q to become ready for %+v; Didn't happen", selector, totalWait)
			os.Exit(1)
		}

		podName, err := GetPodName(kubeClient, namespace, selector)
		if err != nil {
			log.Error().Err(err).Msgf("Error getting Pod w/ selector %q", selector)
			time.Sleep(WaitForPod)
			// Pod might not be up yet, try again
			continue
		}

		pod, err := kubeClient.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			log.Error().Err(err).Msgf("Error getting pod %s/%s", namespace, podName)
			os.Exit(1)
		}

		for _, container := range pod.Status.ContainerStatuses {
			if container.State.Waiting != nil && statusWorthWaitingFor.Contains(container.State.Waiting.Reason) {
				fmt.Printf("Pod %s/%s is still initializing; Waiting %+v (%+v/%+v)\n", namespace, podName, WaitForPod, time.Since(startedWaiting), totalWait)
				time.Sleep(WaitForPod)
				continue
			}

			log.Info().Msgf("Pod %q is ready!", podName)
			wg.Done()
			return
		}
	}
}
