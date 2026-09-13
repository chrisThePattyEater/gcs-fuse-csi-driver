#!/bin/bash

# Copyright 2018 The Kubernetes Authors.
# Copyright 2022 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -o errexit
set -o nounset
set -o pipefail

echo "Verifying gomod..."
export GO111MODULE=on

PKG_ROOT=$(git rev-parse --show-toplevel)

# This repo has two modules that both vendor their dependencies: the driver at the
# root and the e2e suite in test/. Both have to be verified. A dependency bump that
# only syncs the root module leaves test/vendor stale, which the GitHub CI build
# does not notice because it compiles the e2e suite with -mod=readonly, but which
# fails the prow e2e jobs, where ginkgo compiles with the default -mod=vendor.
for module in "." "test"; do
  echo "go mod tidy (${module})"
  go -C "${PKG_ROOT}/${module}" mod tidy
  echo "go mod vendor (${module})"
  go -C "${PKG_ROOT}/${module}" mod vendor
done

diff=`git diff`
if [[ -n "${diff}" ]]; then
  echo "${diff}"
  echo
  echo "error"
  exit 1
fi
echo "No issue found."