package util

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	obv1 "github.com/kube-object-storage/lib-bucket-provisioner/pkg/apis/objectbucket.io/v1alpha1"
	nbapis "github.com/noobaa/noobaa-operator/pkg/apis"
	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	ctx        = context.TODO()
	log        = logrus.WithContext(ctx)
	lazyConfig *rest.Config
	lazyRest   *rest.RESTClient
	lazyClient client.Client
)

func init() {
	Panic(apiextv1beta1.AddToScheme(scheme.Scheme))
	Panic(nbapis.AddToScheme(scheme.Scheme))
	Panic(obv1.AddToScheme(scheme.Scheme))
}

// KubeConfig loads kubernetes client config from default locations (flags, user dir, etc)
func KubeConfig() *rest.Config {
	if lazyConfig == nil {
		var err error
		lazyConfig, err = config.GetConfig()
		Panic(err)
	}
	return lazyConfig
}

// KubeRest returns a configured kubernetes REST client
func KubeRest() *rest.RESTClient {
	if lazyRest == nil {
		var err error
		config := KubeConfig()
		lazyRest, err = rest.RESTClientFor(config)
		Panic(err)
	}
	return lazyRest
}

// MapperProvider creates RESTMapper
func MapperProvider(config *rest.Config) (meta.RESTMapper, error) {
	return meta.NewLazyRESTMapperLoader(func() (meta.RESTMapper, error) {
		dc, err := discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			return nil, err
		}
		return NewFastRESTMapper(dc, func(g *metav1.APIGroup) bool {
			if g.Name == "" ||
				g.Name == "apps" ||
				g.Name == "noobaa.io" ||
				g.Name == "objectbucket.io" ||
				g.Name == "operator.openshift.io" ||
				g.Name == "cloudcredential.openshift.io" ||
				strings.HasSuffix(g.Name, ".k8s.io") {
				return true
			}
			return false
		}), nil
	}), nil
}

// KubeClient resturns a controller-runtime client
// We use a lazy mapper and a specialized implementation of fast mapper
// in order to avoid lags when running a CLI client to a far away cluster.
func KubeClient() client.Client {
	if lazyClient == nil {
		config := KubeConfig()
		mapper, _ := MapperProvider(config)
		var err error
		lazyClient, err = client.New(config, client.Options{Mapper: mapper, Scheme: scheme.Scheme})
		Panic(err)
	}
	return lazyClient
}

// KubeObject loads a text yaml/json to a kubernets object.
func KubeObject(text string) runtime.Object {
	// Decode text (yaml/json) to kube api object
	deserializer := serializer.NewCodecFactory(scheme.Scheme).UniversalDeserializer()
	obj, group, err := deserializer.Decode([]byte(text), nil, nil)
	// obj, group, err := scheme.Codecs.UniversalDecoder().Decode([]byte(text), nil, nil)
	Panic(err)
	// not sure if really needed, but set it anyway
	obj.GetObjectKind().SetGroupVersionKind(*group)
	return obj
}

// KubeApply will check if the object exists and will create/update accordingly
// and report the object status.
func KubeApply(obj runtime.Object) bool {
	klient := KubeClient()
	objKey := ObjectKey(obj)
	gvk := obj.GetObjectKind().GroupVersionKind()
	clone := obj.DeepCopyObject()
	err := klient.Get(ctx, objKey, clone)
	if err == nil {
		err = klient.Update(ctx, obj)
		if err == nil {
			log.Printf("✅ Updated: %s %q\n", gvk.Kind, objKey.Name)
			return false
		}
	}
	if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		log.Printf("❌ CRD Missing: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	if errors.IsNotFound(err) {
		err = klient.Create(ctx, obj)
		if err == nil {
			log.Printf("✅ Created: %s %q\n", gvk.Kind, objKey.Name)
			return true
		}
	}
	if errors.IsConflict(err) {
		log.Printf("❌ Conflict: %s %q: %s\n", gvk.Kind, objKey.Name, err)
		return false
	}
	Panic(err)
	return false
}

// KubeCreateSkipExisting will check if the object exists and will create/skip accordingly
// and report the object status.
func KubeCreateSkipExisting(obj runtime.Object) bool {
	klient := KubeClient()
	objKey := ObjectKey(obj)
	gvk := obj.GetObjectKind().GroupVersionKind()
	clone := obj.DeepCopyObject()
	err := klient.Get(ctx, objKey, clone)
	if err == nil {
		log.Printf("✅ Already Exists: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		log.Printf("❌ CRD Missing: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	if errors.IsNotFound(err) {
		err = klient.Create(ctx, obj)
		if err == nil {
			log.Printf("✅ Created: %s %q\n", gvk.Kind, objKey.Name)
			return true
		}
		if errors.IsNotFound(err) {
			log.Printf("❌ Namespace Missing: %s %q: kubectl create ns %s\n",
				gvk.Kind, objKey.Name, objKey.Namespace)
			return false
		}
	}
	if errors.IsConflict(err) {
		log.Printf("❌ Conflict: %s %q: %s\n", gvk.Kind, objKey.Name, err)
		return false
	}
	if errors.IsForbidden(err) {
		log.Printf("❌ Forbidden: %s %q: %s\n", gvk.Kind, objKey.Name, err)
		return false
	}
	Panic(err)
	return false
}

// KubeDelete deletes an object and reports the object status.
func KubeDelete(obj runtime.Object) bool {
	klient := KubeClient()
	objKey := ObjectKey(obj)
	gvk := obj.GetObjectKind().GroupVersionKind()
	err := klient.Delete(ctx, obj)
	if err == nil {
		log.Printf("🗑️  Deleted: %s %q\n", gvk.Kind, objKey.Name)
		return true
	}
	if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		log.Printf("❌ CRD Missing: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	if errors.IsConflict(err) {
		log.Printf("❌ Conflict: %s %q: %s\n", gvk.Kind, objKey.Name, err)
		return false
	}
	if errors.IsNotFound(err) {
		log.Printf("🗑️  Not Found: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	Panic(err)
	return false
}

// KubeUpdate updates an object and reports the object status.
func KubeUpdate(obj runtime.Object) bool {
	klient := KubeClient()
	objKey := ObjectKey(obj)
	gvk := obj.GetObjectKind().GroupVersionKind()
	err := klient.Update(ctx, obj)
	if err == nil {
		log.Printf("✅ Updated: %s %q\n", gvk.Kind, objKey.Name)
		return true
	}
	if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		log.Printf("❌ CRD Missing: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	if errors.IsConflict(err) {
		log.Printf("❌ Conflict: %s %q: %s\n", gvk.Kind, objKey.Name, err)
		return false
	}
	if errors.IsNotFound(err) {
		log.Printf("❌ Not Found: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	Panic(err)
	return false
}

// KubeCheck checks if the object exists and reports the object status.
func KubeCheck(obj runtime.Object) bool {
	klient := KubeClient()
	objKey := ObjectKey(obj)
	gvk := obj.GetObjectKind().GroupVersionKind()
	err := klient.Get(ctx, objKey, obj)
	if err == nil {
		log.Printf("✅ Exists: %s %q\n", gvk.Kind, objKey.Name)
		return true
	}
	if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		log.Printf("❌ CRD Missing: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	if errors.IsNotFound(err) {
		log.Printf("❌ Not Found: %s %q\n", gvk.Kind, objKey.Name)
		return false
	}
	if errors.IsConflict(err) {
		log.Printf("❌ Conflict: %s %q: %s\n", gvk.Kind, objKey.Name, err)
		return false
	}
	Panic(err)
	return false
}

// KubeList returns a list of objects.
func KubeList(list runtime.Object, options *client.ListOptions) bool {
	klient := KubeClient()
	gvk := list.GetObjectKind().GroupVersionKind()
	err := klient.List(ctx, options, list)
	if err == nil {
		return true
	}
	if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		log.Printf("❌ CRD Missing: %s: %s\n", gvk.Kind, err)
		return false
	}
	if errors.IsNotFound(err) {
		log.Printf("❌ Not Found: %s\n", gvk.Kind)
		return false
	}
	if errors.IsConflict(err) {
		log.Printf("❌ Conflict: %s: %s\n", gvk.Kind, err)
		return false
	}
	Panic(err)
	return false
}

// RemoveFinalizer modifies the object and removes the finalizer
func RemoveFinalizer(obj metav1.Object, finalizer string) bool {
	finalizers := obj.GetFinalizers()
	if finalizers == nil {
		return false
	}
	// see https://github.com/golang/go/wiki/SliceTricks#filter-in-place
	n := 0
	found := false
	for _, f := range finalizers {
		if f == finalizer {
			found = true
		} else {
			finalizers[n] = f
			n++
		}
	}
	finalizers = finalizers[:n]
	obj.SetFinalizers(finalizers)
	return found
}

// GetPodStatusLine returns a one liner status for a pod
func GetPodStatusLine(pod *corev1.Pod) string {
	s := fmt.Sprintf("Phase=%q. ", pod.Status.Phase)
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Status != corev1.ConditionTrue {
			s += fmt.Sprintf("%s (%s). ", c.Reason, c.Message)
		}
	}
	return s
}

// GetContainerStatusLine returns a one liner status for a container
func GetContainerStatusLine(cont *corev1.ContainerStatus) string {
	s := ""
	if cont.RestartCount > 0 {
		s += fmt.Sprintf("RestartCount=%d. ", cont.RestartCount)
	}
	if cont.State.Waiting != nil {
		s += fmt.Sprintf("%s (%s). ", cont.State.Waiting.Reason, cont.State.Waiting.Message)
	}
	if cont.State.Terminated != nil {
		s += fmt.Sprintf("%s (%s). ", cont.State.Terminated.Reason, cont.State.Terminated.Message)
	}
	if s == "" {
		s = "starting..."
	}
	return s
}

// Panic is conviniently calling panic only if err is not nil
func Panic(err error) {
	if err != nil {
		reason := errors.ReasonForError(err)
		log.Panicf("☠️  Panic Attack: [%s] %s", reason, err)
	}
}

// InitLogger initializes the logrus logger with defaults
func InitLogger() {
	logrus.SetLevel(logrus.DebugLevel)
	logrus.SetFormatter(&logrus.TextFormatter{
		// FullTimestamp: true,
	})
}

// Logger returns a default logger
func Logger() *logrus.Entry {
	return log
}

// Context returns a default Context
func Context() context.Context {
	return ctx
}

// CurrentNamespace reads the current namespace from the kube config
func CurrentNamespace() string {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	ns, _, err := kubeConfig.Namespace()
	Panic(err)
	return ns
}

// PersistentError is an error type that tells the reconcile to avoid requeueing.
type PersistentError struct {
	E error
}

// Error function makes PersistentError implement error interface
func (e *PersistentError) Error() string { return e.E.Error() }

// assert implement error interface
var _ error = &PersistentError{}

// NewPersistentError returns a new persistent error.
func NewPersistentError(err error) *PersistentError {
	if err == nil {
		panic("NewPersistentError expects non nil error")
	}
	return &PersistentError{E: err}
}

// IsPersistentError checks if the provided error is persistent.
func IsPersistentError(err error) bool {
	_, isPersistent := err.(*PersistentError)
	return isPersistent
}

// CombineErrors takes a list of errors and combines them to one.
// Generally it will return the first non-nil error,
// but if a persistent error is found it will be returned
// instead of non-persistent errors.
func CombineErrors(errs ...error) error {
	combined := error(nil)
	for _, err := range errs {
		if err == nil {
			continue
		}
		if combined == nil {
			combined = err
			continue
		}
		if IsPersistentError(err) && !IsPersistentError(combined) {
			combined = err
		}
	}
	return combined
}

// SecretResetStringDataFromData reads the secret data into string data
// to streamline the paths that use the secret values as strings.
func SecretResetStringDataFromData(secret *corev1.Secret) {
	secret.StringData = map[string]string{}
	for key, val := range secret.Data {
		secret.StringData[key] = string(val)
	}
	secret.Data = map[string][]byte{}
}

// ObjectKey returns the objects key (namespace + name)
func ObjectKey(obj runtime.Object) client.ObjectKey {
	objKey, err := client.ObjectKeyFromObject(obj)
	Panic(err)
	return objKey
}

// RandomBase64 creates a random buffer with numBytes and returns it encoded in base64
// Returned string length is 4*math.Ceil(numBytes/3)
func RandomBase64(numBytes int) string {
	randomBytes := make([]byte, numBytes)
	_, err := rand.Read(randomBytes)
	Panic(err)
	return base64.StdEncoding.EncodeToString(randomBytes)
}

// RandomHex creates a random buffer with numBytes and returns it encoded in hex
// Returned string length is 2*numBytes
func RandomHex(numBytes int) string {
	randomBytes := make([]byte, numBytes)
	_, err := rand.Read(randomBytes)
	Panic(err)
	return hex.EncodeToString(randomBytes)
}

// SetErrorCondition updates the status conditions to error state
func SetErrorCondition(conditions *[]conditionsv1.Condition, err error) {
	reason := "ReconcileFailed"
	message := fmt.Sprintf("Error while reconciling: %v", err)
	currentTime := metav1.NewTime(time.Now())
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		//LastHeartbeatTime should be set by the custom-resource-status just like lastTransitionTime
		// Setting it here temporarity
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionAvailable,
		Status:            corev1.ConditionUnknown,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionProgressing,
		Status:            corev1.ConditionFalse,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionDegraded,
		Status:            corev1.ConditionTrue,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionUpgradeable,
		Status:            corev1.ConditionUnknown,
		Reason:            reason,
		Message:           message,
	})
}

// SetAvailableCondition updates the status conditions to available state
func SetAvailableCondition(conditions *[]conditionsv1.Condition, reason string, message string) {
	currentTime := metav1.NewTime(time.Now())
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionAvailable,
		Status:            corev1.ConditionTrue,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionProgressing,
		Status:            corev1.ConditionFalse,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionDegraded,
		Status:            corev1.ConditionFalse,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionUpgradeable,
		Status:            corev1.ConditionTrue,
		Reason:            reason,
		Message:           message,
	})
}

// SetProgressingCondition updates the status conditions to in-progress state
func SetProgressingCondition(conditions *[]conditionsv1.Condition, reason string, message string) {
	currentTime := metav1.NewTime(time.Now())
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionAvailable,
		Status:            corev1.ConditionFalse,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionProgressing,
		Status:            corev1.ConditionTrue,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionDegraded,
		Status:            corev1.ConditionFalse,
		Reason:            reason,
		Message:           message,
	})
	conditionsv1.SetStatusCondition(conditions, conditionsv1.Condition{
		LastHeartbeatTime: currentTime,
		Type:              conditionsv1.ConditionUpgradeable,
		Status:            corev1.ConditionFalse,
		Reason:            reason,
		Message:           message,
	})
}
