#!/usr/bin/env bash

set -exuo pipefail

echo "hello"

config_args="--config=crosslinux --config=ci"
# --config ci
# --config force_build_cdeps
# -c opt

go_test_targets=$(bazel query kind\(go_test, //pkg/sql/...:all\))
stage_dir="./stage"
rm -rf $stage_dir

# bazel clean --expunge
bazel build $config_args $go_test_targets
#export bazel_bin_info=$(bazel info bazel-bin $config_args)

#bazel_bin=_bazel/bin
bazel_bin=$(bazel info bazel-bin $config_args)

for pkg in $go_test_targets; do
  # Do something with each item
  echo "Processing: $pkg"
  path=$(echo "$pkg" | cut -d ':' -f 1 | cut -c 3-)  # Extract everything before the colon
  test_name=$(echo "$pkg" | cut -d ':' -f 2)  # Extract everything after the colon

  echo "Path: $path"
  echo "Test name: $test_name"

  ls -l $bazel_bin/$path/${test_name}_/

  mkdir -p $stage_dir/$path/bin

  ln -s $bazel_bin/$path/${test_name}_/${test_name}.runfiles $stage_dir/$path/bin/
  ln -s $bazel_bin/$path/${test_name}_/${test_name} $stage_dir/$path/bin/${test_name}

  #ln -s #{.BazelOutputDir#}/#{.TargetBinName#} #{.StageDir#}/#{.TargetBinName#}
done

mkdir -p /artifacts/stage
# cp -r $stage_dir/* /artifacts/stage
XZ_OPT='-T0 -6' tar -chzf /artifacts/stage.tar.xz $stage_dir/*

# ls $bazel_bin_info

# exec bash -l

# ls -R $bazel_bin

# bash
