#!/usr/bin/env bash
set -euo pipefail

registry=wgdl-registry-vpc.cn-shenzhen.cr.aliyuncs.com
instance=cri-kvnxde6ro98iwv71
repo=crr-5ilbcanqv1mvr13q
base_repo=crr-idm75d20sru5nehm
cluster=c4c464237565b494fb8c73f7893a032c2
image="$registry/wg/model-gateway"
sha=${GITHUB_SHA:?}
[[ "$sha" =~ ^[0-9a-f]{40}$ && "$(git rev-parse HEAD)" == "$sha" ]]

umask 077
credentials=$(mktemp -d)
trap 'rm -rf "$credentials"' EXIT
export DOCKER_CONFIG="$credentials/docker"
export KUBECONFIG="$credentials/kubeconfig"

cr() {
  aliyun cr "$@" --InstanceId "$instance" --region cn-shenzhen
}

acr_tag() {
  cr GetRepoTag --RepoId "$1" --Tag "$2"
}

acr_digest() {
  jq -er '
    (.Digest // "" | sub("^sha256:"; "")) as $digest
    | select(.Code == "success" and (.Status // "NORMAL") == "NORMAL")
    | $digest | select(test("^[a-f0-9]{64}$")) | "sha256:" + .
  '
}

tag_info=$(acr_tag "$repo" "$sha")
# ACR 拒绝覆盖不可变 tag，重跑时复用原 digest。只有明确不存在才构建。
if [[ "$(jq -r '.Code' <<<"$tag_info")" == IMAGE_NOT_EXIST ]]; then
  builder=$(acr_tag "$base_repo" golang-1.26-alpine | acr_digest)
  runtime=$(acr_tag "$base_repo" alpine-3.23 | acr_digest)
  auth=$(cr GetAuthorizationToken)
  user=$(jq -er '.TempUsername | select(length > 0)' <<<"$auth")
  jq -er '.AuthorizationToken | select(length > 0)' <<<"$auth" \
    | docker login "$registry" --username "$user" --password-stdin >/dev/null
  unset auth
  docker build --platform linux/amd64 --provenance=false \
    --build-arg "GO_BUILDER_IMAGE=$registry/wg/base-images@$builder" \
    --build-arg "GO_RUNTIME_IMAGE=$registry/wg/base-images@$runtime" \
    --build-arg GOPROXY=https://goproxy.cn,direct --build-arg "SOURCE_REVISION=$sha" \
    --tag "$image:$sha" .
  docker push "$image:$sha"
  # push 成功后，ACR 的 tag 查询可能晚几秒才可见。
  for attempt in {1..10}; do
    tag_info=$(acr_tag "$repo" "$sha")
    [[ "$(jq -r '.Code' <<<"$tag_info")" == IMAGE_NOT_EXIST ]] || break
    sleep 3
  done
fi
digest=$(acr_digest <<<"$tag_info")

aliyun cs GET "/k8s/$cluster/user_config" --region cn-shenzhen --TemporaryDurationMinutes 15 \
  | jq -er '.config | select(length > 0)' > "$KUBECONFIG"
kube=(kubectl --namespace default)
deployment=$("${kube[@]}" get deployment/model-gateway -o json)

# 构建可能落后于后续 push，发布前核对 dev 分支以免旧任务回退镜像。
latest=$(git ls-remote --exit-code origin refs/heads/dev | cut -f1)
if [[ "$latest" != "$sha" ]]; then
  echo "Skipping superseded dev commit $sha"
  exit 0
fi

# 镜像和 workspace 读取的版本标记在同一个 patch 更新。
patch=$(jq -cn --arg image "$image@$digest" --arg digest "$digest" --arg sha "$sha" \
  --arg rv "$(jq -er '.metadata.resourceVersion' <<<"$deployment")" '{
    metadata: {resourceVersion: $rv, annotations: {
      "ops.wgdl.tech/git-sha": $sha, "ops.wgdl.tech/image-digest": $digest
    }},
    spec: {template: {spec: {containers: [{name: "model-gateway", image: $image}]}}}
  }')
"${kube[@]}" patch deployment/model-gateway --type strategic --patch "$patch"
"${kube[@]}" rollout status deployment/model-gateway --timeout=300s

desired=$("${kube[@]}" get deployment/model-gateway -o json | jq -er '
  .spec.replicas as $want
  | select($want > 0 and .status.observedGeneration >= .metadata.generation
    and .status.readyReplicas == $want and .status.updatedReplicas == $want
    and .status.availableReplicas == $want)
  | $want
')
"${kube[@]}" get pods -l app=model-gateway -o json | jq -e \
  --argjson desired "$desired" --arg digest "$digest" '
    [.items[] | select(.metadata.deletionTimestamp == null)] as $pods
    | ($pods | length) == $desired
      and all($pods[]; .status.phase == "Running" and any(.status.containerStatuses[]?;
        .name == "model-gateway" and .ready and (.imageID | endswith($digest))))
  ' >/dev/null
"${kube[@]}" get --raw /api/v1/namespaces/default/services/http:model-gateway:http/proxy/_gateway/ready \
  | jq -e '.ready == true' >/dev/null
printf 'cn-dev sha=%s digest=%s ready_replicas=%s\n' "$sha" "$digest" "$desired"
