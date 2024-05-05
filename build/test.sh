#!/usr/bin/env bash

set -exuo pipefail

export root="/Users/herko/go/src/github.com/cockroachdb/cockroach"

source "teamcity-support.sh"  # For run_bazel
source "teamcity-bazel-support.sh"  # For run_bazel

if tc_release_branch; then
  echo "On release branch"
else
  echo "Not on release branch"
fi

#BAZEL_SUPPORT_EXTRA_DOCKER_ARGS="-e LITERAL_ARTIFACTS_DIR=$root/artifacts -e BUILD_VCS_NUMBER -e CLOUD -e COCKROACH_DEV_LICENSE -e TESTS -e COUNT -e GITHUB_API_TOKEN -e GITHUB_ORG -e GITHUB_REPO -e GOOGLE_EPHEMERAL_CREDENTIALS -e GOOGLE_KMS_KEY_A -e GOOGLE_KMS_KEY_B -e GOOGLE_CREDENTIALS_ASSUME_ROLE -e GOOGLE_SERVICE_ACCOUNT -e SLACK_TOKEN -e TC_BUILDTYPE_ID -e TC_BUILD_BRANCH -e TC_BUILD_ID -e TC_SERVER_URL -e SELECT_PROBABILITY -e COCKROACH_RANDOM_SEED -e ROACHTEST_ASSERTIONS_ENABLED_SEED -e ROACHTEST_FORCE_RUN_INVALID_RELEASE_BRANCH -e GRAFANA_SERVICE_ACCOUNT_JSON -e GRAFANA_SERVICE_ACCOUNT_AUDIENCE -e ARM_PROBABILITY" \
#			       run_bazel build/teamcity/cockroach/nightlies/roachtest_nightly_impl.sh

BAZEL_SUPPORT_EXTRA_DOCKER_ARGS="-e LITERAL_ARTIFACTS_DIR=$root/artifacts" \
  run_bazel build/test_build.sh

