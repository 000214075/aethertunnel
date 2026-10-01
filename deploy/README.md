# deploy/

部署清单：[`kubernetes/`](kubernetes/) 是单节点集群的完整清单（namespace、secret 示例、
configmap、deployment、service、kustomization），`scripts/kubernetes-linux.sh` 用它在
真实 k3s 上跑通 14 项端到端检查——清单不是手写的摆设，是被脚本反复部署验证过的。

Deployment manifests: [`kubernetes/`](kubernetes/) holds the complete set for a
single-node cluster, and `scripts/kubernetes-linux.sh` deploys it onto a real k3s and
runs 14 end-to-end checks against it — the manifests are exercised, not decorative.
