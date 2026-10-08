# deploy/

部署清单。[`kubernetes/`](kubernetes/) 是单节点集群可用的整套（namespace、Secret 示例、
ConfigMap、Deployment、Service、kustomization），不是需要自己拼的示范片段；清单的渲染校验用
`kubectl kustomize`，要把它真正 apply 到集群并驱动一遍，用
[`scripts/kubernetes-linux.sh`](../scripts/kubernetes-linux.sh)（可选，需要 root、docker 与
一个 k3s 二进制，见 [`docs/PLATFORMS.md`](../docs/PLATFORMS.md)）。

Deployment manifests. [`kubernetes/`](kubernetes/) is the whole set for a single-node
cluster — namespace, Secret example, ConfigMap, Deployment, Service, kustomization — not
fragments to assemble; render-check them with `kubectl kustomize`, and to apply the set to
a cluster and drive it, use [`scripts/kubernetes-linux.sh`](../scripts/kubernetes-linux.sh)
(optional; it needs root, docker and a k3s binary).

清单里两件事值得注意：

- **凭据走 Secret，配置走 ConfigMap**。`configmap.yaml` 里的 `server.toml` 单独校验
  **通不过**（它故意不写 `auth_token`），带上环境变量里的令牌才通过并真的跑起来——
  这样"把令牌写进 ConfigMap"这种错法在校验时就会暴露。
- **状态落在 emptyDir 上**。审计日志、带宽账本与签名密钥写在 Deployment 挂的卷里，
  Pod 被替换时就一起没了——要长期保留就先把它换成 PersistentVolumeClaim。

Two things in the manifests are deliberate: credentials come from a Secret, and the
`server.toml` in the ConfigMap deliberately fails validation on its own, so writing a
token into the ConfigMap is caught; and the audit log, the ledger and the signing
keys live on the `emptyDir` the Deployment mounts, so they are lost when the Pod is
replaced — swap it for a PersistentVolumeClaim before relying on them.
