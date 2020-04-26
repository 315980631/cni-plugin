make all
docker build -t calico/cni:v3.12.0-fix --build-arg GIT_VERSION=1.8.3.1 -f Dockerfile .
