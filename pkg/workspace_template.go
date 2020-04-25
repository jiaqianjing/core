package v1

import (
	"fmt"
	wfv1 "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	v1 "github.com/onepanelio/core/pkg/apis/core/v1"
	"github.com/onepanelio/core/pkg/util/ptr"
	networking "istio.io/api/networking/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func parseWorkspaceSpec(template string) (spec *v1.WorkspaceSpec, err error) {
	err = yaml.UnmarshalStrict([]byte(template), &spec)

	return
}

func generateArguments(spec *v1.WorkspaceSpec, config map[string]string) (err error) {
	if spec.Arguments == nil {
		spec.Arguments = &v1.Arguments{
			Parameters: []v1.Parameter{},
		}
	}

	// Resource action parameter
	spec.Arguments.Parameters = append(spec.Arguments.Parameters, v1.Parameter{
		Name:     "op-name",
		Type:     "input.text",
		Value:    "name",
		Required: true,
	})

	// Resource action parameter
	spec.Arguments.Parameters = append(spec.Arguments.Parameters, v1.Parameter{
		Name:  "op-resource-action",
		Value: "apply",
		Type:  "input.hidden",
	})

	// Workspace action
	spec.Arguments.Parameters = append(spec.Arguments.Parameters, v1.Parameter{
		Name:  "op-workspace-action",
		Value: "create",
		Type:  "input.hidden",
	})

	// Node pool parameter and options
	var options []*v1.ParameterOption
	if err = yaml.Unmarshal([]byte(config["applicationNodePoolOptions"]), &options); err != nil {
		return
	}
	spec.Arguments.Parameters = append(spec.Arguments.Parameters, v1.Parameter{
		Name:     "op-node-pool",
		Value:    options[0].Value,
		Type:     "select.select",
		Options:  options,
		Required: true,
	})

	// Volume size parameters
	volumeClaimsMapped := make(map[string]bool)
	for _, c := range spec.Containers {
		for _, v := range c.VolumeMounts {
			if volumeClaimsMapped[v.Name] {
				continue
			}

			spec.Arguments.Parameters = append(spec.Arguments.Parameters, v1.Parameter{
				Name:     fmt.Sprintf("op-%v-volume-size", v.Name),
				Type:     "input.number",
				Value:    "20480",
				Required: true,
			})

			volumeClaimsMapped[v.Name] = true
		}
	}

	return
}

func createServiceManifest(spec *v1.WorkspaceSpec) (serviceManifest string, err error) {
	service := corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "{{workflow.parameters.op-name}}",
		},
		Spec: corev1.ServiceSpec{
			Ports: spec.Ports,
			Selector: map[string]string{
				"app": "{{workflow.parameters.op-name}}",
			},
		},
	}
	serviceManifestBytes, err := yaml.Marshal(service)
	if err != nil {
		return
	}
	serviceManifest = string(serviceManifestBytes)

	return
}

func createVirtualServiceManifest(spec *v1.WorkspaceSpec, config map[string]string) (virtualServiceManifest string, err error) {
	for _, h := range spec.Routes {
		for _, r := range h.Route {
			r.Destination.Host = "{{workflow.parameters.op-name}}"
		}
	}
	virtualService := map[string]interface{}{
		"apiVersion": "networking.istio.io/v1alpha3",
		"kind":       "VirtualService",
		"metadata": metav1.ObjectMeta{
			Name: "{{workflow.parameters.op-name}}",
		},
		"spec": networking.VirtualService{
			Http:     spec.Routes,
			Gateways: []string{"istio-system/ingressgateway"},
			Hosts:    []string{fmt.Sprintf("{{workflow.parameters.op-name}}-{{workflow.namespace}}.%v", config["ONEPANEL_HOST"])},
		},
	}
	virtualServiceManifestBytes, err := yaml.Marshal(virtualService)
	if err != nil {
		return
	}
	virtualServiceManifest = string(virtualServiceManifestBytes)

	return
}

func createStatefulSetManifest(workspaceSpec *v1.WorkspaceSpec, config map[string]string) (statefulSetManifest string, err error) {
	var volumeClaims []map[string]interface{}
	volumeClaimsMapped := make(map[string]bool)
	for _, c := range workspaceSpec.Containers {
		for _, v := range c.VolumeMounts {
			if volumeClaimsMapped[v.Name] {
				continue
			}

			volumeClaims = append(volumeClaims, map[string]interface{}{
				"metadata": metav1.ObjectMeta{
					Name: v.Name,
				},
				"spec": map[string]interface{}{
					"accessModes": []corev1.PersistentVolumeAccessMode{
						"ReadWriteOnce",
					},
					"storageClassName": ptr.String("onepanel"),
					"resources": map[string]interface{}{
						"requests": map[string]string{
							"storage": fmt.Sprintf("{{workflow.parameters.op-%v-volume-size}}Mi", v.Name),
						},
					},
				},
			})

			volumeClaimsMapped[v.Name] = true
		}
	}

	statefulSet := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": metav1.ObjectMeta{
			Name: "{{workflow.parameters.op-name}}",
		},
		"spec": map[string]interface{}{
			"replicas":    1,
			"serviceName": "{{workflow.parameters.op-name}}",
			"selector": &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "{{workflow.parameters.op-name}}",
				},
			},
			"template": corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "{{workflow.parameters.op-name}}",
					},
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						config["applicationNodePoolLabel"]: "{{workflow.parameters.op-node-pool}}",
					},
					Containers: workspaceSpec.Containers,
				},
			},
			"volumeClaimTemplates": volumeClaims,
		},
	}
	statefulSetManifestBytes, err := yaml.Marshal(statefulSet)
	if err != nil {
		return
	}
	statefulSetManifest = string(statefulSetManifestBytes)

	return
}

func unmarshalWorkflowTemplate(spec *v1.WorkspaceSpec, serviceManifest, virtualServiceManifest, containersManifest string) (workflowTemplateSpecManifest string, err error) {
	var volumeClaimItems []wfv1.Item
	volumeClaimsMapped := make(map[string]bool)
	for _, c := range spec.Containers {
		for _, v := range c.VolumeMounts {
			if volumeClaimsMapped[v.Name] {
				continue
			}

			volumeClaimItems = append(volumeClaimItems, wfv1.Item{Type: wfv1.String, StrVal: v.Name})

			volumeClaimsMapped[v.Name] = true
		}
	}

	deletePVCManifest := `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{inputs.parameters.op-pvc-name}}-{{workflow.parameters.op-name}}-0
`

	// TODO: Consider storing this as a Go template in a "settings" database table
	workflowTemplateSpec := map[string]interface{}{
		"arguments":  spec.Arguments,
		"entrypoint": "workspace",
		"templates": []wfv1.Template{
			{
				Name: "workspace",
				DAG: &wfv1.DAGTemplate{
					Tasks: []wfv1.DAGTask{
						{
							Name:     "service",
							Template: "service-resource",
						},
						{
							Name:         "virtual-service",
							Template:     "virtual-service-resource",
							Dependencies: []string{"service"},
						},
						{
							Name:         "stateful-set",
							Template:     "stateful-set-resource",
							Dependencies: []string{"virtual-service"},
							When:         "{{workflow.parameters.op-workspace-action}} == create || {{workflow.parameters.op-workspace-action}} == update",
						},
						{
							Name:         "delete-stateful-set",
							Template:     "delete-stateful-set-resource",
							Dependencies: []string{"virtual-service"},
							When:         "{{workflow.parameters.op-workspace-action}} == pause || {{workflow.parameters.op-workspace-action}} == delete",
						},
						{
							Name:         "delete-pvc",
							Template:     "delete-pvc-resource",
							Dependencies: []string{"delete-stateful-set"},
							Arguments: wfv1.Arguments{
								Parameters: []wfv1.Parameter{
									{
										Name:  "op-pvc-name",
										Value: ptr.String("{{item}}"),
									},
								},
							},
							When:      "{{workflow.parameters.op-workspace-action}} == delete",
							WithItems: volumeClaimItems,
						},
					},
				},
			},
			{
				Name: "service-resource",
				Resource: &wfv1.ResourceTemplate{
					Action:   "{{workflow.parameters.op-resource-action}}",
					Manifest: serviceManifest,
				},
			},
			{
				Name: "virtual-service-resource",
				Resource: &wfv1.ResourceTemplate{
					Action:   "{{workflow.parameters.op-resource-action}}",
					Manifest: virtualServiceManifest,
				},
			},
			{
				Name: "stateful-set-resource",
				Resource: &wfv1.ResourceTemplate{
					Action:           "{{workflow.parameters.op-resource-action}}",
					Manifest:         containersManifest,
					SuccessCondition: "status.readyReplicas > 0",
				},
			},
			{
				Name: "delete-stateful-set-resource",
				Resource: &wfv1.ResourceTemplate{
					Action:   "{{workflow.parameters.op-resource-action}}",
					Manifest: containersManifest,
				},
			},
			{
				Name: "delete-pvc-resource",
				Inputs: wfv1.Inputs{
					Parameters: []wfv1.Parameter{{Name: "op-pvc-name"}},
				},
				Resource: &wfv1.ResourceTemplate{
					Action:   "{{workflow.parameters.op-resource-action}}",
					Manifest: deletePVCManifest,
				},
			},
		},
	}
	workflowTemplateSpecManifestBytes, err := yaml.Marshal(workflowTemplateSpec)
	if err != nil {
		return
	}
	workflowTemplateSpecManifest = string(workflowTemplateSpecManifestBytes)

	return
}

// CreateWorkspaceTemplate creates a template for Workspaces
func (c *Client) CreateWorkspaceTemplate(namespace string, workspaceTemplate *WorkspaceTemplate) (*WorkspaceTemplate, error) {
	config, err := c.GetSystemConfig()
	if err != nil {
		return nil, err
	}

	workspaceSpec, err := parseWorkspaceSpec(workspaceTemplate.Manifest)
	if err != nil {
		return nil, err
	}

	if err = generateArguments(workspaceSpec, config); err != nil {
		return nil, err
	}

	serviceManifest, err := createServiceManifest(workspaceSpec)
	if err != nil {
		return nil, err
	}

	virtualServiceManifest, err := createVirtualServiceManifest(workspaceSpec, config)
	if err != nil {
		return nil, err
	}

	containersManifest, err := createStatefulSetManifest(workspaceSpec, config)
	if err != nil {
		return nil, err
	}

	workflowTemplateManifest, err := unmarshalWorkflowTemplate(workspaceSpec, serviceManifest, virtualServiceManifest, containersManifest)
	if err != nil {
		return nil, err
	}

	workspaceTemplate.WorkflowTemplate, err = c.CreateWorkflowTemplate(namespace, &WorkflowTemplate{
		Name:     workspaceTemplate.Name,
		Manifest: string(workflowTemplateManifest),
	})

	return workspaceTemplate, nil
}
