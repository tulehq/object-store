#!/bin/sh
# Runs IMAGE the way users do and checks it works: the container must pass its
# own health check, have created its startup bucket, and store an object on
# its volume and serve it back to signed requests.
set -eu

image=${1:?usage: run.sh IMAGE}
name=object-store-smoke-$$

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		docker logs "$name" || true
	fi
	docker rm -f "$name" >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT

docker run -d --name "$name" -p 127.0.0.1::9000 \
	-e TULE_Auth_AccessKeyID=smoke -e TULE_Auth_SecretAccessKey=smoke \
	-e TULE_Storage_Buckets=smoke \
	"$image" >/dev/null

# The health check first runs one interval (10s) after start.
health=
for _ in $(seq 60); do
	health=$(docker inspect -f '{{.State.Status}} {{.State.Health.Status}}' "$name")
	case $health in
	"running healthy") break ;;
	running*) sleep 1 ;;
	*) echo "container stopped: $health" >&2; exit 1 ;;
	esac
done
if [ "$health" != "running healthy" ]; then
	echo "container never became healthy: $health" >&2
	exit 1
fi

# Volumes from earlier images are owned by 65532:65532.
ids=$(docker exec "$name" id -u):$(docker exec "$name" id -g)
if [ "$ids" != 65532:65532 ]; then
	echo "runs as $ids, want 65532:65532" >&2
	exit 1
fi

addr=$(docker port "$name" 9000/tcp | head -n 1)
s3() {
	curl -fsS --aws-sigv4 aws:amz:us-east-1:s3 --user smoke:smoke "$@"
}

s3 -X PUT --data-binary hello "http://$addr/smoke/hello.txt"
body=$(s3 "http://$addr/smoke/hello.txt")
if [ "$body" != hello ]; then
	echo "read back \"$body\", want \"hello\"" >&2
	exit 1
fi

echo "smoke test passed: $image"
