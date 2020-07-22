package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	// needed for k8s oidc and gcp auth
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

func init() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
	rand.Seed(time.Now().UnixNano())
}

var letters = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

func randSeq(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

type claimInfo struct {
	ownerNode             string
	claim                 *corev1.PersistentVolumeClaim
	readOnly              bool
	svcType               corev1.ServiceType
	deleteExtraneousFiles bool
}

func doCleanup(kubeClient *kubernetes.Clientset, instance string, namespace string) {
	log.WithFields(log.Fields{
		"instance":  instance,
		"namespace": namespace,
	}).Info("Doing cleanup")

	_ = kubeClient.BatchV1().Jobs(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: "app=pv-migrate,instance=" + instance,
	})

	_ = kubeClient.CoreV1().Pods(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: "app=pv-migrate,instance=" + instance,
	})

	_ = kubeClient.CoreV1().Secrets(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: "app=pv-migrate,instance=" + instance,
	})

	serviceClient := kubeClient.CoreV1().Services(namespace)
	serviceList, _ := serviceClient.List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=pv-migrate,instance=" + instance,
	})

	for _, service := range serviceList.Items {
		_ = serviceClient.Delete(context.TODO(), service.Name, metav1.DeleteOptions{})
	}
	log.WithFields(log.Fields{
		"instance": instance,
	}).Info("Finished cleanup")
}

func buildConfigFromFlags(context, kubeconfigPath string) (*rest.Config, error) {
	clientcmd.NewDefaultClientConfigLoadingRules()
	clientConfigLoadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	if kubeconfigPath != "" {
		clientConfigLoadingRules = &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientConfigLoadingRules,
		&clientcmd.ConfigOverrides{
			CurrentContext: context,
		}).ClientConfig()
}

func main() {
	kubeconfig := flag.String("kubeconfig", "", "(optional) absolute path to the kubeconfig file")
	source := flag.String("source", "", "Source persistent volume claim")
	sourceNamespace := flag.String("source-namespace", "", "Source namespace")
	sourceContext := flag.String("source-context", "", "(optional) Source context")
	dest := flag.String("dest", "", "Destination persistent volume claim")
	destNamespace := flag.String("dest-namespace", "", "Destination namespace")
	destContext := flag.String("dest-context", "", "(optional) Destination context")
	sourceReadOnly := flag.Bool("sourceReadOnly", true, "(optional) source pvc ReadOnly")
	deleteExtraneousFromDest := flag.Bool("dest-delete-extraneous-files", false, "(optional) delete extraneous files from destination dirs")
	flag.Parse()

	if *deleteExtraneousFromDest {
		log.Warn("delete extraneous files from dest is enabled")
	}

	if *source == "" || *sourceNamespace == "" || *dest == "" || *destNamespace == "" {
		flag.Usage()
		return
	}

	svcType := corev1.ServiceTypeClusterIP
	if *sourceContext != *destContext {
		svcType = corev1.ServiceTypeLoadBalancer
	}

	sourceCfg, err := buildConfigFromFlags(*sourceContext, *kubeconfig)
	if err != nil {
		log.WithError(err).Fatal("Error building kubeconfig")
	}

	sourceKubeClient, err := kubernetes.NewForConfig(sourceCfg)
	if err != nil {
		log.WithError(err).Fatal("Error building kubernetes clientset")
	}

	destCfg, err := buildConfigFromFlags(*destContext, *kubeconfig)
	if err != nil {
		log.WithError(err).Fatal("Error building kubeconfig")
	}

	destKubeClient, err := kubernetes.NewForConfig(destCfg)
	if err != nil {
		log.WithError(err).Fatal("Error building kubernetes clientset")
	}

	sourceClaimInfo := buildClaimInfo(sourceKubeClient, sourceNamespace, source, *sourceReadOnly, false, svcType)
	destClaimInfo := buildClaimInfo(destKubeClient, destNamespace, dest, false, *deleteExtraneousFromDest, svcType)

	log.Info("Both claims exist and bound, proceeding...")
	instance := randSeq(5)

	handleSigterm(sourceKubeClient, destKubeClient, instance, *sourceNamespace, *destNamespace)

	defer doCleanup(sourceKubeClient, instance, *sourceNamespace)
	defer doCleanup(destKubeClient, instance, *destNamespace)

	err = migrateViaRsync(instance, sourceKubeClient, destKubeClient, sourceClaimInfo, destClaimInfo)
	if err != nil {
		log.WithError(err).Fatal("unable to run migrate")
	}
}

func handleSigterm(sourceKubeClient, destKubeClient *kubernetes.Clientset, instance string, sourceNamespace string, destNamespace string) {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		doCleanup(sourceKubeClient, instance, sourceNamespace)
		doCleanup(destKubeClient, instance, destNamespace)
		os.Exit(1)
	}()
}

func buildRsyncCommand(claimInfo claimInfo, targetHost string) []string {
	rsyncCommand := []string{"rsync"}
	if claimInfo.deleteExtraneousFiles {
		rsyncCommand = append(rsyncCommand, "--delete")
	}
	rsyncCommand = append(rsyncCommand, "-avz")
	rsyncCommand = append(rsyncCommand, fmt.Sprintf("root@%s:/source/", targetHost))
	rsyncCommand = append(rsyncCommand, "/dest/")

	return rsyncCommand
}

func prepareRsyncJob(instance string, destClaimInfo claimInfo, targetHost string) batchv1.Job {
	jobTtlSeconds := int32(600)
	backoffLimit := int32(0)
	jobName := "pv-migrate-rsync-" + instance

	var secureFileMode, normalFileMode int32
	secureFileMode = 0600
	normalFileMode = 0600

	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: destClaimInfo.claim.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &jobTtlSeconds,
			Template: corev1.PodTemplateSpec{

				ObjectMeta: metav1.ObjectMeta{
					Name:      jobName,
					Namespace: destClaimInfo.claim.Namespace,
					Labels: map[string]string{
						"app":       "pv-migrate",
						"component": "rsync",
						"instance":  instance,
					},
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "dest-vol",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: destClaimInfo.claim.Name,
									ReadOnly:  destClaimInfo.readOnly,
								},
							},
						},
						{
							Name: "ssh-keys",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "pv-migrate-" + instance,
									Items: []corev1.KeyToPath{
										{Key: "user-key", Mode: &secureFileMode, Path: "user-key"},
										{Key: "user-pub", Mode: &secureFileMode, Path: "user-pub"},
										{Key: "host-pub", Mode: &normalFileMode, Path: "host-pub"},
										{Key: "host-key", Mode: &secureFileMode, Path: "host-key"},
									},
								}},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "app",
							Image:           "docker.io/utkuozdemir/pv-migrate-rsync:v0.1.0",
							ImagePullPolicy: corev1.PullAlways,
							Command:         buildRsyncCommand(destClaimInfo, targetHost),
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "dest-vol",
									MountPath: "/dest",
									ReadOnly:  destClaimInfo.readOnly,
								},
								{
									Name:      "ssh-keys",
									MountPath: "/root/.ssh/id_ecdsa",
									SubPath:   "user-key",
									ReadOnly:  true,
								},
								{
									Name:      "ssh-keys",
									MountPath: "/root/.ssh/known_hosts",
									SubPath:   "host-pub",
									ReadOnly:  true,
								},
							},
						},
					},
					NodeName:      destClaimInfo.ownerNode,
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	return job
}

func generateSecretData(targetServiceAddress string) (map[string]string, error) {
	secretData := make(map[string]string, 0)
	hostKey, hostPub, err := generateSSHKeyPair()
	if err != nil {
		return nil, err
	}
	userKey, userPub, err := generateSSHKeyPair()
	if err != nil {
		return nil, err
	}
	secretData["host-key"] = hostKey
	secretData["host-pub"] = targetServiceAddress + " " + hostPub
	secretData["user-key"] = userKey
	secretData["user-pub"] = userPub

	return secretData, nil
}

func migrateViaRsync(instance string, sourcekubeClient *kubernetes.Clientset, destkubeClient *kubernetes.Clientset, sourceClaimInfo claimInfo, destClaimInfo claimInfo) error {
	createdService := createSshdService(instance, sourcekubeClient, sourceClaimInfo)
	targetServiceAddress := getServiceAddress(createdService, sourcekubeClient)
	log.Infof("use service address %s to connect to rsync server", targetServiceAddress)

	secretData, err := generateSecretData(targetServiceAddress)
	if err != nil {
		return err
	}

	createSshdSecret(prepareSshSecret(instance, sourceClaimInfo, secretData), sourcekubeClient, sourceClaimInfo)
	// TODO and context
	if sourceClaimInfo.claim.Namespace != destClaimInfo.claim.Namespace || {
		createSshdSecret(prepareSshSecret(instance, destClaimInfo, secretData), destkubeClient, destClaimInfo)
	}

	sftpPod := prepareSshdPod(instance, sourceClaimInfo)
	createSshdPodWaitTillRunning(sourcekubeClient, sftpPod)

	rsyncJob := prepareRsyncJob(instance, destClaimInfo, targetServiceAddress)
	createJobWaitTillCompleted(destkubeClient, rsyncJob)
	return nil
}

func generateSSHKeyPair() (string, string, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		return "", "", err
	}

	data, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return "", "", err
	}

	privateKeyPEM := &pem.Block{Type: "EC PRIVATE KEY", Bytes: data}

	pub, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", "", err
	}

	return string(pem.EncodeToMemory(privateKeyPEM)), string(ssh.MarshalAuthorizedKey(pub)), nil
}

func prepareSshSecret(instance string, cInfo claimInfo, secretData map[string]string) corev1.Secret {
	secretName := "pv-migrate-" + instance

	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cInfo.claim.Namespace,
			Labels: map[string]string{
				"app":       "pv-migrate",
				"component": "sshd",
				"instance":  instance,
			},
		},
		StringData: secretData,
	}
}

func prepareSshdPod(instance string, sourceClaimInfo claimInfo) corev1.Pod {
	podName := "pv-migrate-sshd-" + instance

	var secureFileMode, normalFileMode int32
	secureFileMode = 0600
	normalFileMode = 0600

	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: sourceClaimInfo.claim.Namespace,
			Labels: map[string]string{
				"app":       "pv-migrate",
				"component": "sshd",
				"instance":  instance,
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "source-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: sourceClaimInfo.claim.Name,
							ReadOnly:  sourceClaimInfo.readOnly,
						},
					},
				},
				{
					Name: "ssh-keys",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: "pv-migrate-" + instance,
							Items: []corev1.KeyToPath{
								{Key: "user-key", Mode: &secureFileMode, Path: "user-key"},
								{Key: "user-pub", Mode: &secureFileMode, Path: "user-pub"},
								{Key: "host-pub", Mode: &normalFileMode, Path: "host-pub"},
								{Key: "host-key", Mode: &secureFileMode, Path: "host-key"},
							},
						}},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "app",
					Image:           "docker.io/utkuozdemir/pv-migrate-sshd:v0.1.0",
					ImagePullPolicy: corev1.PullAlways,
					// Args:            []string{"-o", "LogLevel=DEBUG"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "source-vol",
							MountPath: "/source",
							ReadOnly:  sourceClaimInfo.readOnly,
						},
						{
							Name:      "ssh-keys",
							MountPath: "/etc/ssh/ssh_host_ecdsa_key",
							SubPath:   "host-key",
							ReadOnly:  true,
						},
						{
							Name:      "ssh-keys",
							MountPath: "/root/.ssh/authorized_keys",
							SubPath:   "user-pub",
							ReadOnly:  true,
						},
					},
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: 22,
						},
					},
				},
			},
			NodeName: sourceClaimInfo.ownerNode,
		},
	}
}

func buildClaimInfo(kubeClient *kubernetes.Clientset, sourceNamespace *string, source *string, readOnly, deleteExtraneousFiles bool, svcType corev1.ServiceType) claimInfo {
	claim, err := kubeClient.CoreV1().PersistentVolumeClaims(*sourceNamespace).Get(context.TODO(), *source, v1.GetOptions{})
	if err != nil {
		log.WithError(err).WithField("pvc", *source).Fatal("Failed to get source claim")
	}
	if claim.Status.Phase != corev1.ClaimBound {
		log.Fatal("Source claim not bound")
	}
	ownerNode, err := findOwnerNodeForPvc(kubeClient, claim)
	if err != nil {
		log.Fatal("Could not determine the owner of the source claim")
	}
	return claimInfo{
		ownerNode:             ownerNode,
		claim:                 claim,
		readOnly:              readOnly,
		svcType:               svcType,
		deleteExtraneousFiles: deleteExtraneousFiles,
	}
}

func getServiceAddress(svc *corev1.Service, kubeClient *kubernetes.Clientset) string {
	if svc.Spec.Type == corev1.ServiceTypeClusterIP {
		return svc.Spec.ClusterIP
	}

	for {
		createdService, err := kubeClient.CoreV1().Services(svc.Namespace).Get(context.TODO(), svc.Name, v1.GetOptions{})
		if err != nil {
			log.Fatal("unable to get service")
		}

		if len(createdService.Status.LoadBalancer.Ingress) == 0 {
			sleepInterval := 10 * time.Second
			log.Infof("wait for external ip, sleep %s", sleepInterval)
			time.Sleep(sleepInterval)
			continue
		}
		return createdService.Status.LoadBalancer.Ingress[0].IP
	}
}

func createSshdService(instance string, kubeClient *kubernetes.Clientset, sourceClaimInfo claimInfo) *corev1.Service {
	serviceName := "pv-migrate-sshd-" + instance
	createdService, err := kubeClient.CoreV1().Services(sourceClaimInfo.claim.Namespace).Create(
		context.TODO(),
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: sourceClaimInfo.claim.Namespace,
				Labels: map[string]string{
					"app":       "pv-migrate",
					"component": "sshd",
					"instance":  instance,
				},
			},
			Spec: corev1.ServiceSpec{
				Type: sourceClaimInfo.svcType,
				Ports: []corev1.ServicePort{
					{
						Port:       22,
						TargetPort: intstr.FromInt(22),
					},
				},
				Selector: map[string]string{
					"app":       "pv-migrate",
					"component": "sshd",
					"instance":  instance,
				},
			},
		},
		v1.CreateOptions{},
	)
	if err != nil {
		log.WithError(err).Fatal("service creation failed")
	}
	return createdService
}

func createSshdSecret(secret corev1.Secret, kubeClient *kubernetes.Clientset, cInfo claimInfo) {
	log.WithFields(log.Fields{
		"secretName": secret.Name,
	}).Info("Creating sshd secret")
	_, err := kubeClient.CoreV1().Secrets(cInfo.claim.Namespace).Create(
		context.TODO(),
		&secret,
		v1.CreateOptions{},
	)
	if err != nil {
		log.WithError(err).Fatal("secret creation failed")
	}
}

func createSshdPodWaitTillRunning(kubeClient *kubernetes.Clientset, pod corev1.Pod) *corev1.Pod {
	running := make(chan bool)
	defer close(running)
	stopCh := make(chan struct{})
	defer close(stopCh)
	sharedInformerFactory := informers.NewSharedInformerFactory(kubeClient, 5*time.Second)
	sharedInformerFactory.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old interface{}, new interface{}) {
				newPod := new.(*corev1.Pod)
				if newPod.Namespace == pod.Namespace && newPod.Name == pod.Name {
					switch newPod.Status.Phase {
					case corev1.PodRunning:
						log.WithFields(log.Fields{
							"podName": pod.Name,
						}).Info("sshd pod running")
						running <- true

					case corev1.PodFailed, corev1.PodUnknown:
						log.WithFields(log.Fields{
							"podName": newPod.Name,
						}).Panic("sshd pod failed to start, exiting")
					}
				}
			},
		},
	)
	sharedInformerFactory.Start(stopCh)

	log.WithFields(log.Fields{
		"podName": pod.Name,
	}).Info("Creating sshd pod")
	createdPod, err := kubeClient.CoreV1().Pods(pod.Namespace).Create(context.TODO(), &pod, metav1.CreateOptions{})
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"podName": pod.Name,
		}).Fatal("Failed to create sshd pod")
	}

	log.WithFields(log.Fields{
		"podName": pod.Name,
	}).Info("Waiting for pod to start running")
	<-running

	return createdPod
}

func createJobWaitTillCompleted(kubeClient *kubernetes.Clientset, job batchv1.Job) {
	succeeded := make(chan bool)
	defer close(succeeded)
	stopCh := make(chan struct{})
	defer close(stopCh)
	sharedInformerFactory := informers.NewSharedInformerFactory(kubeClient, 5*time.Second)
	sharedInformerFactory.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old interface{}, new interface{}) {
				newPod := new.(*corev1.Pod)
				if newPod.Namespace == job.Namespace && newPod.Labels["job-name"] == job.Name {
					switch newPod.Status.Phase {
					case corev1.PodSucceeded:
						log.WithFields(log.Fields{
							"jobName": job.Name,
							"podName": newPod.Name,
						}).Info("Job completed...")
						succeeded <- true
					case corev1.PodRunning:
						log.WithFields(log.Fields{
							"jobName": job.Name,
							"podName": newPod.Name,
						}).Info("Job is running ")
					case corev1.PodFailed, corev1.PodUnknown:
						log.WithFields(log.Fields{
							"jobName": job.Name,
							"podName": newPod.Name,
						}).Panic("Job failed, exiting")
					}
				}
			},
		},
	)

	sharedInformerFactory.Start(stopCh)

	log.WithFields(log.Fields{
		"jobName": job.Name,
	}).Info("Creating rsync job")
	_, err := kubeClient.BatchV1().Jobs(job.Namespace).Create(context.TODO(), &job, metav1.CreateOptions{})
	if err != nil {
		log.WithFields(log.Fields{
			"jobName": job.Name,
		}).WithError(err).Fatal("Failed to create rsync job")
	}

	log.WithFields(log.Fields{
		"jobName": job.Name,
	}).Info("Waiting for rsync job to finish")
	<-succeeded
}

func findOwnerNodeForPvc(kubeClient *kubernetes.Clientset, pvc *corev1.PersistentVolumeClaim) (string, error) {
	podList, err := kubeClient.CoreV1().Pods(pvc.Namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	for _, pod := range podList.Items {
		for _, volume := range pod.Spec.Volumes {
			persistentVolumeClaim := volume.PersistentVolumeClaim
			if persistentVolumeClaim != nil && persistentVolumeClaim.ClaimName == pvc.Name {
				return pod.Spec.NodeName, nil
			}
		}
	}

	return "", nil
}
