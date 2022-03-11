package controllers

import (
	pipelinev1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func generatePipelineRun(name, namespace, pvcName, sourceNamespace string) *pipelinev1beta1.PipelineRun {
	return &pipelinev1beta1.PipelineRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: pipelinev1beta1.PipelineRunSpec{
			PipelineSpec: &pipelinev1beta1.PipelineSpec{
				Workspaces: []pipelinev1beta1.PipelineWorkspaceDeclaration{
					{Name: "kubeconfig"},
				},
				Tasks: []pipelinev1beta1.PipelineTask{
					{
						Name: pvcName,
						TaskRef: &pipelinev1beta1.TaskRef{
							Name: "crane-transfer-pvc",
							Kind: pipelinev1beta1.ClusterTaskKind,
						},
						Params: []pipelinev1beta1.Param{
							{
								Name: "src-context",
								Value: pipelinev1beta1.ArrayOrString{
									Type:      pipelinev1beta1.ParamTypeString,
									StringVal: "src",
								},
							},
							{
								Name: "dest-context",
								Value: pipelinev1beta1.ArrayOrString{
									Type:      pipelinev1beta1.ParamTypeString,
									StringVal: "dest",
								},
							},
							{
								Name: "src-namespace",
								Value: pipelinev1beta1.ArrayOrString{
									Type:      pipelinev1beta1.ParamTypeString,
									StringVal: sourceNamespace,
								},
							},
							{
								Name: "pvc-name",
								Value: pipelinev1beta1.ArrayOrString{
									Type:      pipelinev1beta1.ParamTypeString,
									StringVal: pvcName,
								},
							},
							{
								Name: "endpoint-type",
								Value: pipelinev1beta1.ArrayOrString{
									Type:      pipelinev1beta1.ParamTypeString,
									StringVal: "nginx-ingress",
								},
							},
							{
								Name: "dest-namespace",
								Value: pipelinev1beta1.ArrayOrString{
									Type: pipelinev1beta1.ParamTypeString,
									// this is the populator namespace
									StringVal: namespace,
								},
							},
						},
						Workspaces: []pipelinev1beta1.WorkspacePipelineTaskBinding{
							{Name: "kubeconfig", Workspace: "kubeconfig"},
						},
					},
				},
			},
			Workspaces: []pipelinev1beta1.WorkspaceBinding{
				{
					Name: "kubeconfig",
					Secret: &corev1.SecretVolumeSource{
						SecretName: "kubeconfig",
					},
				},
			},
		},
	}
}
