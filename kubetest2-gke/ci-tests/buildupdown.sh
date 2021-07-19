#!/bin/bash

# Copyright 2021 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail
set -o xtrace

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "${REPO_ROOT}" &> /dev/null || exit 1

make install
make install-deployer-gke

# currently equivalent to /home/prow/go/src/github.com/kubernetes/kubernetes
REPO_ROOT="${REPO_ROOT}/../../kubernetes/kubernetes"

function main() {
  CLUSTER_TOPOLOGY="sc"
  BUILD_STRATEGY="make"

  while [ $# -gt 0 ]; do
    case "$1" in
      --cluster-topology)
        shift
        CLUSTER_TOPOLOGY="$1"
        ;;
      --build-strategy)
        shift
        BUILD_STRATEGY="$1"
        ;;
      *)
        echo "Invalid argument"
        exit 1
        ;;
      esac
    shift
  done

  NUM_CLUSTERS=0
  case "${CLUSTER_TOPOLOGY}" in
    "sc")
      NUM_CLUSTERS=1
      ;;
    "mc")
      NUM_CLUSTERS=2
      ;;
    *)
      echo "Invalid cluster topology ${CLUSTER_TOPOLOGY}"
      exit 1
      ;;
  esac

  kubetest2 gke \
    -v 2 \
    --repo-root "$REPO_ROOT" \
    --strategy "${BUILD_STRATEGY}" \
    --stage gs://kubernetes-jenkins/ci \
    --num-clusters "${NUM_CLUSTERS}" \
    --num-nodes 1 \
    --zone us-central1-c,us-west1-a,us-east1-b \
    --build \
    --up \
    --down
}

main "$@"
