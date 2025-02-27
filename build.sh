#!/bin/bash

# 定义要编译的平台
platforms=("windows/amd64" "darwin/amd64" "linux/amd64")

# 获取当前目录名作为输出名称
name=$(basename $(pwd))

# 遍历每个平台并编译
for platform in "${platforms[@]}"
do
    export GOOS=${platform%/*}
    export GOARCH=${platform#*/}
    output="${name}_${GOOS}_${GOARCH}"
    if [ "$GOOS" == "windows" ]; then
        output="${output}.exe"
    fi
    go build -o $output
done