package kustomize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	k8smeta "k8s.io/apimachinery/pkg/api/meta"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func kustomizationResourceSchemaV0() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"manifest": {
			Type:     schema.TypeString,
			Required: true,
		},
	}
}

func kustomizationResourceV0() *schema.Resource {
	return &schema.Resource{
		Schema: kustomizationResourceSchemaV0(),
	}
}

func kustomizationResourceSchemaV1() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"manifest": {
			Type:      schema.TypeString,
			Required:  true,
			Sensitive: true,
		},
		"resource": {
			Type:     schema.TypeString,
			Optional: true,
			Computed: true,
		},

		"secret_data": {
			Type:      schema.TypeString,
			Sensitive: true,
			Optional:  true,
			Computed:  true,
		},
	}
}

func kustomizationResourceV1() *schema.Resource {
	return &schema.Resource{
		Schema: kustomizationResourceSchemaV1(),
	}
}

func kustomizationResource() *schema.Resource {
	return &schema.Resource{
		Create:        kustomizationResourceCreate,
		Read:          kustomizationResourceRead,
		Exists:        kustomizationResourceExists,
		Update:        kustomizationResourceUpdate,
		Delete:        kustomizationResourceDelete,
		CustomizeDiff: kustomizationResourceDiff,

		Importer: &schema.ResourceImporter{
			State: kustomizationResourceImport,
		},

		Schema:        kustomizationResourceSchemaV1(),
		SchemaVersion: 1,
		StateUpgraders: []schema.StateUpgrader{
			{
				Version: 0,
				Upgrade: v1StateUpgradeFunc,
				Type:    kustomizationResourceV0().CoreConfigSchema().ImpliedType(),
			},
		},
	}
}

func v1StateUpgradeFunc(rawState map[string]interface{}, meta interface{}) (map[string]interface{}, error) {
	u, err := parseJSON(rawState["manifest"].(string))
	if err != nil {
		return nil, err
	}
	if u.GetKind() != "Secret" {
		rawState["resource"] = rawState["manifest"]
		rawState["secret_data"] = ""
		return rawState, nil
	}
	rawState["secret_data"], rawState["resource"], err = extractSecretData(u)
	return rawState, err
}

func extractSecretData(u *unstructured.Unstructured) (string, string, error) {
	secretData, ok, err := unstructured.NestedMap(u.Object, "data")
	secret := []byte{}
	var updated *unstructured.Unstructured
	if err != nil {
		return "", "", err
	}
	if ok {
		updated = u.DeepCopy()
		unstructured.SetNestedField(updated.Object, "SENSITIVE", "data")
		secret, err = json.Marshal(secretData)
		if err != nil {
			return "", "", err
		}
	}
	data, err := updated.MarshalJSON()
	if err != nil {
		return "", "", err
	}
	return string(data), string(secret), nil
}

func updateStateFromResponse(d *schema.ResourceData, u *unstructured.Unstructured) error {
	manifest := getLastAppliedConfig(u)
	d.Set("manifest", manifest)

	secretData := ""
	if u.GetKind() == "Secret" {
		configured, err := parseJSON(manifest)
		if err != nil {
			return err
		}
		manifest, secretData, err = extractSecretData(configured)
		if err != nil {
			return err
		}
	}
	d.Set("resource", manifest)
	d.Set("secret_data", secretData)
	return nil
}

func kustomizationResourceCreate(d *schema.ResourceData, m interface{}) error {
	client := m.(*Config).Client
	mapper := m.(*Config).Mapper

	srcJSON := d.Get("manifest").(string)
	u, err := parseJSON(srcJSON)
	if err != nil {
		return logError(fmt.Errorf("JSON parse error: %s", err))
	}

	stateConf := &resource.StateChangeConf{
		Target:  []string{"existing"},
		Pending: []string{"pending"},
		Timeout: d.Timeout(schema.TimeoutCreate),
		Refresh: func() (interface{}, string, error) {
			// CRDs: wait for GroupVersionKind to exist
			mapper.Reset()
			mapping, err := mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
			if err != nil {
				if k8smeta.IsNoMatchError(err) {
					return nil, "pending", nil
				}
				return nil, "", err
			}

			return mapping.Resource, "existing", nil
		},
	}
	gvrResp, err := stateConf.WaitForState()
	if err != nil {
		return logErrorForResource(
			u,
			fmt.Errorf("timed out waiting for apiVersion: %q, kind: %q to exist: %s", u.GroupVersionKind().GroupVersion(), u.GroupVersionKind().Kind, err),
		)
	}

	gvr := gvrResp.(k8sschema.GroupVersionResource)

	namespace := u.GetNamespace()

	setLastAppliedConfig(u, srcJSON)

	if namespace != "" {
		// wait for the namespace to exist
		nsGvk := k8sschema.GroupVersionKind{
			Group:   "",
			Version: "",
			Kind:    "Namespace"}
		mapping, err := mapper.RESTMapping(nsGvk.GroupKind(), nsGvk.GroupVersion().Version)
		if err != nil {
			return logErrorForResource(
				u,
				fmt.Errorf("api server has no apiVersion: %q, kind: %q: %s", nsGvk.GroupVersion(), nsGvk.Kind, err),
			)
		}

		stateConf := &resource.StateChangeConf{
			Target:  []string{"existing"},
			Pending: []string{"pending"},
			Timeout: d.Timeout(schema.TimeoutCreate),
			Refresh: func() (interface{}, string, error) {
				resp, err := client.
					Resource(mapping.Resource).
					Get(context.TODO(), namespace, k8smetav1.GetOptions{})
				if err != nil {
					if k8serrors.IsNotFound(err) {
						return nil, "pending", nil
					}
					return nil, "", err
				}

				return resp, "existing", nil
			},
		}
		_, err = stateConf.WaitForState()
		if err != nil {
			return logErrorForResource(
				u,
				fmt.Errorf("timed out waiting for apiVersion: %q, kind: %q, name: %q, to exist: %s", nsGvk.GroupVersion(), nsGvk.Kind, namespace, err),
			)
		}
	}

	resp, err := client.
		Resource(gvr).
		Namespace(namespace).
		Create(context.TODO(), u, k8smetav1.CreateOptions{})
	if err != nil {
		return logErrorForResource(
			u,
			fmt.Errorf("create failed: %s", err),
		)
	}

	id := string(resp.GetUID())
	d.SetId(id)

	return kustomizationResourceRead(d, m)
}

func kustomizationResourceRead(d *schema.ResourceData, m interface{}) error {
	client := m.(*Config).Client
	mapper := m.(*Config).Mapper

	srcJSON := d.Get("manifest").(string)
	u, err := parseJSON(srcJSON)
	if err != nil {
		return logError(fmt.Errorf("JSON parse error: %s", err))
	}

	mapping, err := mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
	if err != nil {
		return logErrorForResource(
			u,
			fmt.Errorf("failed to query GVR: %s", err),
		)
	}

	resp, err := client.
		Resource(mapping.Resource).
		Namespace(u.GetNamespace()).
		Get(context.TODO(), u.GetName(), k8smetav1.GetOptions{})
	if err != nil {
		return logErrorForResource(
			u,
			fmt.Errorf("get failed: %s", err),
		)
	}

	id := string(resp.GetUID())
	d.SetId(id)

	updateStateFromResponse(d, resp)

	return nil
}

func kustomizationResourceDiff(ctx context.Context, d *schema.ResourceDiff, m interface{}) error {
	client := m.(*Config).Client
	mapper := m.(*Config).Mapper

	originalJSON, modifiedJSON := d.GetChange("manifest")

	modifiedSrcJSON := modifiedJSON.(string)
	mu, err := parseJSON(modifiedSrcJSON)
	if err != nil {
		return logError(fmt.Errorf("JSON parse error: %s", err))
	}
	if mu.GetKind() == "Secret" {
		cleanedResource, secretData, err := extractSecretData(mu)
		if err != nil {
			return logError(fmt.Errorf("Couldn't extract secret: %s", err))
		}
		d.SetNew("resource", string(cleanedResource))
		d.SetNew("secret_data", string(secretData))
	} else {
		d.SetNew("resource", modifiedSrcJSON)
		d.SetNew("secret_data", "")
	}

	originalSrcJSON := originalJSON.(string)
	if originalSrcJSON == "" {
		return nil
	}

	ou, err := parseJSON(originalSrcJSON)
	if err != nil {
		return logError(fmt.Errorf("JSON parse error: %s", err))
	}

	if ou.GetName() != mu.GetName() || ou.GetNamespace() != mu.GetNamespace() {
		// if the resource name or namespace changes, we can't patch but have to destroy and re-create
		d.ForceNew("manifest")
		return nil
	}

	mapping, err := mapper.RESTMapping(mu.GroupVersionKind().GroupKind(), mu.GroupVersionKind().Version)
	if err != nil {
		return logErrorForResource(
			mu,
			fmt.Errorf("failed to query GVR: %s", err),
		)
	}

	original, modified, current, err := getOriginalModifiedCurrent(
		originalJSON.(string),
		modifiedJSON.(string),
		true,
		m)
	if err != nil {
		return logErrorForResource(
			mu,
			fmt.Errorf("getOriginalModifiedCurrent failed: %s", err),
		)
	}

	patch, patchType, err := getPatch(mu.GroupVersionKind(), original, modified, current)
	if err != nil {
		return logErrorForResource(
			mu,
			fmt.Errorf("getPatch failed: %s", err),
		)
	}

	dryRunPatch := k8smetav1.PatchOptions{DryRun: []string{k8smetav1.DryRunAll}}

	_, err = client.
		Resource(mapping.Resource).
		Namespace(mu.GetNamespace()).
		Patch(context.TODO(), mu.GetName(), patchType, patch, dryRunPatch)
	if err != nil {
		// Handle specific invalid errors
		if k8serrors.IsInvalid(err) {
			as := err.(k8serrors.APIStatus).Status()

			// ForceNew only when exact single cause
			if len(as.Details.Causes) == 1 {
				msg := as.Details.Causes[0].Message

				// if cause is immutable field force a delete and re-create plan
				if k8serrors.HasStatusCause(err, k8smetav1.CauseTypeFieldValueInvalid) && strings.HasSuffix(msg, ": field is immutable") == true {
					d.ForceNew("manifest")
					return nil
				}

				// if cause is statefulset forbidden fields error force a delete and re-create plan
				if k8serrors.HasStatusCause(err, k8smetav1.CauseType(field.ErrorTypeForbidden)) && strings.HasPrefix(msg, "Forbidden: updates to statefulset spec for fields") == true {
					d.ForceNew("manifest")
					return nil
				}

			}
		}

		return logErrorForResource(
			mu,
			fmt.Errorf("patch failed '%s': %s", patchType, err),
		)
	}

	return nil
}

func kustomizationResourceExists(d *schema.ResourceData, m interface{}) (bool, error) {
	client := m.(*Config).Client
	mapper := m.(*Config).Mapper

	srcJSON := d.Get("manifest").(string)
	u, err := parseJSON(srcJSON)
	if err != nil {
		return false, logError(fmt.Errorf("JSON parse error: %s", err))
	}

	mappings, err := mapper.RESTMappings(u.GroupVersionKind().GroupKind())
	if err != nil {
		if k8smeta.IsNoMatchError(err) {
			// If the Kind does not exist in the K8s API,
			// the resource can't exist either
			return false, logError(fmt.Errorf("Can't find kind %s in API group %s", u.GroupVersionKind().Kind, u.GroupVersionKind().Group))
		}
		return false, err
	}

	_, err = client.
		Resource(mappings[0].Resource).
		Namespace(u.GetNamespace()).
		Get(context.TODO(), u.GetName(), k8smetav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false, nil
		}
		return false, logErrorForResource(
			u,
			fmt.Errorf("get failed: %s", err),
		)
	}

	return true, nil
}

func kustomizationResourceUpdate(d *schema.ResourceData, m interface{}) error {
	client := m.(*Config).Client
	mapper := m.(*Config).Mapper

	originalJSON, modifiedJSON := d.GetChange("manifest")

	srcJSON := originalJSON.(string)
	ou, err := parseJSON(srcJSON)
	if err != nil {
		return logError(fmt.Errorf("JSON parse error: %s", err))
	}

	if !d.HasChange("manifest") {
		return logErrorForResource(
			ou,
			errors.New("update called without diff"),
		)
	}

	modifiedSrcJSON := modifiedJSON.(string)
	mu, err := parseJSON(modifiedSrcJSON)
	if err != nil {
		return logError(fmt.Errorf("JSON parse error: %s", err))
	}

	mapping, err := mapper.RESTMapping(mu.GroupVersionKind().GroupKind(), mu.GroupVersionKind().Version)
	if err != nil {
		return logErrorForResource(
			mu,
			fmt.Errorf("failed to query GVR: %s", err),
		)
	}

	original, modified, current, err := getOriginalModifiedCurrent(
		originalJSON.(string),
		modifiedJSON.(string),
		false,
		m)
	if err != nil {
		return logErrorForResource(
			mu,
			fmt.Errorf("getOriginalModifiedCurrent failed: %s", err),
		)
	}

	patch, patchType, err := getPatch(mu.GroupVersionKind(), original, modified, current)
	if err != nil {
		return logErrorForResource(
			mu,
			fmt.Errorf("getPatch failed: %s", err),
		)
	}

	var patchResp *unstructured.Unstructured
	patchResp, err = client.
		Resource(mapping.Resource).
		Namespace(mu.GetNamespace()).
		Patch(context.TODO(), mu.GetName(), patchType, patch, k8smetav1.PatchOptions{})
	if err != nil {
		return logErrorForResource(
			mu,
			fmt.Errorf("patch failed '%s': %s", patchType, err),
		)
	}

	id := string(patchResp.GetUID())
	d.SetId(id)

	updateStateFromResponse(d, patchResp)

	return kustomizationResourceRead(d, m)
}

func kustomizationResourceDelete(d *schema.ResourceData, m interface{}) error {
	client := m.(*Config).Client
	mapper := m.(*Config).Mapper

	srcJSON := d.Get("manifest").(string)
	u, err := parseJSON(srcJSON)
	if err != nil {
		return logError(fmt.Errorf("JSON parse error: %s", err))
	}

	// look for all versions of the GroupKind in case the resource uses a
	// version that is no longer current
	mappings, err := mapper.RESTMappings(u.GroupVersionKind().GroupKind())
	if err != nil {
		if k8smeta.IsNoMatchError(err) {
			// If the Kind does not exist in the K8s API,
			// the resource can't exist either
			return nil
		}
		return err
	}

	namespace := u.GetNamespace()
	name := u.GetName()

	err = client.
		Resource(mappings[0].Resource).
		Namespace(namespace).
		Delete(context.TODO(), name, k8smetav1.DeleteOptions{})
	if err != nil {
		// Consider not found during deletion a success
		if k8serrors.IsNotFound(err) {
			d.SetId("")
			return nil
		}

		return logErrorForResource(
			u,
			fmt.Errorf("delete failed : %s", err),
		)
	}

	stateConf := &resource.StateChangeConf{
		Target:  []string{},
		Pending: []string{"deleting"},
		Timeout: d.Timeout(schema.TimeoutDelete),
		Refresh: func() (interface{}, string, error) {
			resp, err := client.
				Resource(mappings[0].Resource).
				Namespace(namespace).
				Get(context.TODO(), name, k8smetav1.GetOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					return nil, "", nil
				}
				return nil, "", err
			}

			return resp, "deleting", nil
		},
	}
	_, err = stateConf.WaitForState()
	if err != nil {
		return logErrorForResource(
			u,
			fmt.Errorf("timed out waiting for delete: %s", err),
		)
	}

	d.SetId("")

	return nil
}

func kustomizationResourceImport(d *schema.ResourceData, m interface{}) ([]*schema.ResourceData, error) {
	client := m.(*Config).Client
	mapper := m.(*Config).Mapper

	k, err := parseEitherIdFormat(d.Id())
	if err != nil {
		return nil, logError(err)
	}
	gk := k8sschema.GroupKind{Group: k.Group, Kind: k.Kind}

	// We don't need to use a specific API version here, as we're going to store the
	// resource using the LastAppliedConfig information which we can get from any
	// API version
	mappings, err := mapper.RESTMappings(gk)
	if err != nil {
		return nil, logError(
			fmt.Errorf("group: %q, kind: %q, namespace: %q, name: %q: failed to query GVR: %s", gk.Group, gk.Kind, k.Namespace, k.Name, err),
		)
	}

	resp, err := client.
		Resource(mappings[0].Resource).
		Namespace(k.Namespace).
		Get(context.TODO(), k.Name, k8smetav1.GetOptions{})
	if err != nil {
		return nil, logError(
			fmt.Errorf("group: %q, kind: %q, namespace: %q, name: %q: get failed: %s", gk.Group, gk.Kind, k.Namespace, k.Name, err),
		)
	}

	id := string(resp.GetUID())
	d.SetId(id)

	lac := getLastAppliedConfig(resp)
	if lac == "" {
		return nil, logError(
			fmt.Errorf("group: %q, kind: %q, namespace: %q, name: %q: can not import resources without %q annotation", gk.Group, gk.Kind, k.Namespace, k.Name, lastAppliedConfig),
		)
	}

	updateStateFromResponse(d, resp)

	return []*schema.ResourceData{d}, nil
}
