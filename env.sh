# v2panel 构建环境 (source 后使用)
export HOME=/root
export PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export GOPATH=/root/go
export GOMODCACHE=/root/go/pkg/mod
export GOCACHE=/root/.cache/go-build
export GOPROXY=https://goproxy.cn,direct
export GOFLAGS=-p=1
export GOMAXPROCS=2
export CGO_ENABLED=0
export GOTOOLCHAIN=local
