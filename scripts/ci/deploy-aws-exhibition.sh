#!/usr/bin/env bash
set -euo pipefail
export AWS_REGION=us-east-2 AWS_DEFAULT_REGION=us-east-2 AWS_PAGER=

registry=054043816891.dkr.ecr.us-east-2.amazonaws.com
repo=wg/model-gateway
image="$registry/$repo"
stack=tomirro-model-gateway
cluster=tomirro
service=model-gateway-service
service_arn="arn:aws:ecs:us-east-2:054043816891:service/$cluster/$service"
sha=${GITHUB_SHA:?}
[[ "$sha" =~ ^[0-9a-f]{40}$ && "$(git rev-parse HEAD)" == "$sha" ]]
tag="git-$sha"

check_dev_head() {
  local latest
  latest=$(git ls-remote --exit-code origin refs/heads/dev | cut -f1)
  if [[ "$latest" != "$sha" ]]; then
    echo "Skipping superseded dev commit $sha"
    exit 0
  fi
}

umask 077
export DOCKER_CONFIG
DOCKER_CONFIG=$(mktemp -d)
change_set=
cleanup() {
  if [[ -n "$change_set" ]]; then
    aws cloudformation delete-change-set --stack-name "$stack" --change-set-name "$change_set" || true
  fi
  rm -rf "$DOCKER_CONFIG"
}
trap cleanup EXIT

lookup_image() {
  aws ecr batch-get-image --repository-name "$repo" --image-ids "imageTag=$tag"
}

check_dev_head
image_info=$(lookup_image)
# 复用按完整 SHA 发布的镜像，授权或查询失败时直接结束。
if [[ "$(jq -r '.failures[0].failureCode // empty' <<<"$image_info")" == ImageNotFound ]]; then
  aws ecr get-login-password | docker login --username AWS --password-stdin "$registry" >/dev/null
  docker build --platform linux/amd64 --provenance=false \
    --build-arg "SOURCE_REVISION=$sha" --tag "$image:$tag" .
  docker push "$image:$tag"
  image_info=$(lookup_image)
fi
digest=$(jq -er '.images[0].imageId.imageDigest | select(test("^sha256:[0-9a-f]{64}$"))' <<<"$image_info")

service_state=$(aws ecs describe-services --cluster "$cluster" --services "$service" --include TAGS)
jq -e --arg arn "$service_arn" '
  .services[0] | .serviceArn == $arn and .desiredCount > 0
    and any(.tags[]?; .key == "Environment" and .value == "ppe_exhibition")
' <<<"$service_state" >/dev/null
owner=$(aws cloudformation describe-stack-resource --stack-name "$stack" --logical-resource-id Service \
  --query StackResourceDetail.PhysicalResourceId --output text)
[[ "$owner" == "$service_arn" ]]
stack_state=$(aws cloudformation describe-stacks --stack-name "$stack" --query 'Stacks[0]')
jq -e '.StackStatus | test("^(CREATE_COMPLETE|UPDATE_COMPLETE|UPDATE_ROLLBACK_COMPLETE)$")' \
  <<<"$stack_state" >/dev/null
previous=$(jq -er '.Parameters[] | select(.ParameterKey == "ImageDigest") | .ParameterValue' <<<"$stack_state")

if [[ "$previous" != "$digest" ]]; then
  # 沿用路演栈的现有模板及参数，只更新 ImageDigest 并限制变更集的资源范围。
  params=$(jq -c --arg digest "$digest" '.Parameters | map(
    if .ParameterKey == "ImageDigest" then {ParameterKey, ParameterValue: $digest}
    else {ParameterKey, UsePreviousValue: true} end
  )' <<<"$stack_state")
  change_name="gha-model-gateway-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
  aws cloudformation create-change-set --stack-name "$stack" --change-set-name "$change_name" \
    --change-set-type UPDATE --use-previous-template --parameters "$params" \
    --capabilities CAPABILITY_NAMED_IAM >/dev/null
  change_set=$change_name
  aws cloudformation wait change-set-create-complete --stack-name "$stack" --change-set-name "$change_set"
  changes=$(aws cloudformation describe-change-set --stack-name "$stack" --change-set-name "$change_set" \
    --query 'Changes[].ResourceChange.{id:LogicalResourceId,action:Action,type:ResourceType,replacement:Replacement}')
  jq -e '
    length == 2
    and any(.[]; .id == "Task" and .action == "Modify" and .type == "AWS::ECS::TaskDefinition")
    and any(.[]; .id == "Service" and .action == "Modify" and .type == "AWS::ECS::Service" and .replacement == "False")
  ' <<<"$changes" >/dev/null || { echo "Unexpected exhibition stack changes: $changes" >&2; exit 1; }
  check_dev_head
  aws cloudformation execute-change-set --stack-name "$stack" --change-set-name "$change_set"
  change_set=
  aws cloudformation wait stack-update-complete --stack-name "$stack"
fi

aws ecs wait services-stable --cluster "$cluster" --services "$service"
service_state=$(aws ecs describe-services --cluster "$cluster" --services "$service" --query 'services[0]')
desired=$(jq -er '
  .desiredCount as $want
  | select($want > 0 and .runningCount == $want and .pendingCount == 0
    and (.deployments | length) == 1 and .deployments[0].rolloutState == "COMPLETED")
  | $want
' <<<"$service_state")
revision=$(jq -er '.taskDefinition' <<<"$service_state")
task_arns=$(aws ecs list-tasks --cluster "$cluster" --service-name "$service" \
  --desired-status RUNNING --query taskArns)
jq -e --argjson desired "$desired" 'length == $desired' <<<"$task_arns" >/dev/null
# 服务稳定也可能是回滚到旧版后的状态，必须核对每个运行任务的 digest 和健康状态。
aws ecs describe-tasks --cluster "$cluster" --tasks "$task_arns" | jq -e \
  --argjson desired "$desired" --arg revision "$revision" --arg digest "$digest" '
    (.failures | length) == 0 and (.tasks | length) == $desired
    and all(.tasks[]; .taskDefinitionArn == $revision and .lastStatus == "RUNNING" and .healthStatus == "HEALTHY"
      and any(.containers[]; .name == "model-gateway" and .healthStatus == "HEALTHY" and .imageDigest == $digest))
  ' >/dev/null
printf 'ppe_exhibition sha=%s digest=%s healthy_tasks=%s task_definition=%s\n' "$sha" "$digest" "$desired" "$revision"
