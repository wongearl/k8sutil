package apply

import (
	"bytes"
	"context"
	"fmt"
	"github.com/wongearl/k8sutil/util"
	"io"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/klog/v2"
)

const DefaultDecoderBufferSize = 500

type applyOptions struct {
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	serverSide      bool
}
type FailedObject struct {
	Name       string
	Kind       string
	ErrMessage string
}

func NewApplyOptions(dynamicClient dynamic.Interface, discoveryClient discovery.DiscoveryInterface) *applyOptions {
	return &applyOptions{
		dynamicClient:   dynamicClient,
		discoveryClient: discoveryClient,
	}
}

func (o *applyOptions) WithServerSide(serverSide bool) *applyOptions {
	o.serverSide = serverSide
	return o
}

func (o *applyOptions) ToRESTMapper() (meta.RESTMapper, error) {
	gr, err := restmapper.GetAPIGroupResources(o.discoveryClient)
	if err != nil {
		return nil, err
	}

	mapper := restmapper.NewDiscoveryRESTMapper(gr)
	return mapper, nil
}

func (o *applyOptions) Apply(ctx context.Context, data []byte) (failedObjects []FailedObject, err error) {
	restMapper, err := o.ToRESTMapper()
	if err != nil {
		return failedObjects, err
	}

	unstructList, err := Decode(data)
	if err != nil {
		return failedObjects, err
	}

	for _, unstruct := range unstructList {
		klog.V(5).Infof("Apply object: %#v", unstruct)
		if object, err := ApplyUnstructured(ctx, o.dynamicClient, restMapper, unstruct, o.serverSide); err != nil {
			// 如果apply该对象出错，跳过该对象的apply，继续下一个对象的apply
			tmpFailedObject := FailedObject{Name: object.GetName(), Kind: object.GetKind(), ErrMessage: err.Error()}
			failedObjects = append(failedObjects, tmpFailedObject)
		}
		klog.V(2).Infof("%s/%s applyed", strings.ToLower(unstruct.GetKind()), unstruct.GetName())
	}

	return failedObjects, err
}

func Decode(data []byte) ([]unstructured.Unstructured, error) {
	var err error
	var unstructList []unstructured.Unstructured
	i := 1

	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), DefaultDecoderBufferSize)
	for {
		var reqObj runtime.RawExtension
		if err = decoder.Decode(&reqObj); err != nil {
			break
		}
		klog.V(5).Infof("The section:[%d] raw content: %s", i, string(reqObj.Raw))

		obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(reqObj.Raw, nil, nil)
		if err != nil {
			err = fmt.Errorf("serialize the section:[%d] content error, %v", i, err)
			break
		}
		klog.V(5).Infof("The section:[%d] GroupVersionKind: %#v  object: %#v", i, gvk, obj)

		unstruct, err := util.ConvertSingleObjectToUnstructured(obj)
		if err != nil {
			err = fmt.Errorf("serialize the section:[%d] content error, %v", i, err)
			break
		}
		unstructList = append(unstructList, unstruct)
		i++
	}

	if err != io.EOF {
		return unstructList, fmt.Errorf("parsing the section:[%d] content error, %v", i, err)
	}

	return unstructList, nil
}

func ApplyUnstructured(ctx context.Context, dynamicClient dynamic.Interface, restMapper meta.RESTMapper, unstructuredObj unstructured.Unstructured, serverSide bool) (*unstructured.Unstructured, error) {

	if len(unstructuredObj.GetName()) == 0 {
		metadata, _ := meta.Accessor(unstructuredObj)
		generateName := metadata.GetGenerateName()
		if len(generateName) > 0 {
			return nil, fmt.Errorf("from %s: cannot use generate name with apply", generateName)
		}
	}

	b, err := unstructuredObj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(b, nil, nil)
	if err != nil {
		return nil, err
	}

	mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("mapping: %v", mapping.Scope.Name())

	var dri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if unstructuredObj.GetNamespace() == "" {
			unstructuredObj.SetNamespace("default")
		}
		dri = dynamicClient.Resource(mapping.Resource).Namespace(unstructuredObj.GetNamespace())
	} else {
		dri = dynamicClient.Resource(mapping.Resource)
	}

	if serverSide {
		klog.V(2).Infof("Using server-side apply")
		if _, ok := unstructuredObj.GetAnnotations()[corev1.LastAppliedConfigAnnotation]; ok {
			annotations := unstructuredObj.GetAnnotations()
			delete(annotations, corev1.LastAppliedConfigAnnotation)
			unstructuredObj.SetAnnotations(annotations)
		}
		unstructuredObj.SetManagedFields(nil)
		klog.V(4).Infof("Need remove managedFields before apply, %#v", unstructuredObj)

		force := true
		opts := metav1.PatchOptions{FieldManager: "k8sutil", Force: &force}
		if _, err := dri.Patch(ctx, unstructuredObj.GetName(), types.ApplyPatchType, b, opts); err != nil {
			if isIncompatibleServerError(err) {
				err = fmt.Errorf("server-side apply not available on the server: (%v)", err)
			}
			return nil, err
		}
		return nil, nil
	}

	modified, err := util.GetModifiedConfiguration(obj, true, unstructured.UnstructuredJSONScheme)
	if err != nil {
		return nil, fmt.Errorf("retrieving modified configuration from:\n%s\nfor:%v", unstructuredObj.GetName(), err)
	}

	currentUnstr, err := dri.Get(ctx, unstructuredObj.GetName(), metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("retrieving current configuration of:\n%s\nfrom server for:%v", unstructuredObj.GetName(), err)
		}

		klog.V(2).Infof("The resource %s creating", unstructuredObj.GetName())
		// Create the resource if it doesn't exist
		// First, update the annotation such as kubectl apply
		if err := util.CreateApplyAnnotation(&unstructuredObj, unstructured.UnstructuredJSONScheme); err != nil {
			return nil, fmt.Errorf("creating %s error: %v", unstructuredObj.GetName(), err)
		}

		return dri.Create(ctx, &unstructuredObj, metav1.CreateOptions{})
	}

	klog.V(2).Infof("The resource %s apply", unstructuredObj.GetName())
	metadata, _ := meta.Accessor(currentUnstr)
	annotationMap := metadata.GetAnnotations()
	if _, ok := annotationMap[corev1.LastAppliedConfigAnnotation]; !ok {
		klog.Warningf("[%s] apply should be used on resource created by either kubectl create --save-config or apply", metadata.GetName())
	}

	patchBytes, patchType, err := Patch(currentUnstr, modified, unstructuredObj.GetName(), *gvk)
	if err != nil {
		return nil, err
	}
	return dri.Patch(ctx, unstructuredObj.GetName(), patchType, patchBytes, metav1.PatchOptions{})
}

func Patch(currentUnstr *unstructured.Unstructured, modified []byte, name string, gvk schema.GroupVersionKind) ([]byte, types.PatchType, error) {
	current, err := currentUnstr.MarshalJSON()
	if err != nil {
		return nil, "", fmt.Errorf("serializing current configuration from: %v, %v", currentUnstr, err)
	}

	original, err := util.GetOriginalConfiguration(currentUnstr)
	if err != nil {
		return nil, "", fmt.Errorf("retrieving original configuration from: %s, %v", name, err)
	}

	var patchType types.PatchType
	var patch []byte

	versionedObject, err := Scheme.New(gvk)
	switch {
	case runtime.IsNotRegisteredError(err):
		patchType = types.MergePatchType
		preconditions := []mergepatch.PreconditionFunc{
			mergepatch.RequireKeyUnchanged("apiVersion"),
			mergepatch.RequireKeyUnchanged("kind"),
			mergepatch.RequireKeyUnchanged("name"),
		}
		patch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(original, modified, current, preconditions...)
		if err != nil {
			if mergepatch.IsPreconditionFailed(err) {
				return nil, "", fmt.Errorf("At least one of apiVersion, kind and name was changed")
			}
			return nil, "", fmt.Errorf("unable to apply patch, %v", err)
		}
	case err == nil:
		patchType = types.StrategicMergePatchType
		lookupPatchMeta, err := strategicpatch.NewPatchMetaFromStruct(versionedObject)
		if err != nil {
			return nil, "", err
		}
		patch, err = strategicpatch.CreateThreeWayMergePatch(original, modified, current, lookupPatchMeta, true)
		if err != nil {
			return nil, "", err
		}
	case err != nil:
		return nil, "", fmt.Errorf("getting instance of versioned object %v for: %v", gvk, err)
	}

	return patch, patchType, nil
}

func ObjectToUnstructured(obj runtime.Object) ([]unstructured.Unstructured, error) {
	list := make([]unstructured.Unstructured, 0, 0)
	if meta.IsListType(obj) {
		if _, ok := obj.(*unstructured.UnstructuredList); !ok {
			return nil, fmt.Errorf("unable to convert runtime object to list")
		}

		for _, u := range obj.(*unstructured.UnstructuredList).Items {
			list = append(list, u)
		}
		return list, nil
	}

	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}

	unstructuredObj := unstructured.Unstructured{Object: unstructuredMap}
	list = append(list, unstructuredObj)

	return list, nil
}

func isIncompatibleServerError(err error) bool {
	if _, ok := err.(*errors.StatusError); !ok {
		return false
	}
	// 415 说明服务端不支持server-side-apply
	return err.(*errors.StatusError).Status().Code == http.StatusUnsupportedMediaType
}
