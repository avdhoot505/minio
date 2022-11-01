#!/bin/sh -e

DOCKER_REGISTRY="${DOCKER_REGISTRY:-localhost:5000}"
ENVIRONMENT="${ENVIRONMENT:-development}"
BITBUCKET_BRANCH="${BITBUCKET_BRANCH:-${ENVIRONMENT}}"
BITBUCKET_COMMIT="${BITBUCKET_COMMIT:-}"
BITBUCKET_REPO_SLUG="${BITBUCKET_REPO_SLUG:-$(basename $PWD)}"


DOCKER_COMMIT_TAG="$DOCKER_REGISTRY/truyo-io:${BITBUCKET_BRANCH}-$BITBUCKET_COMMIT-$(TZ=':America/Phoenix' date +%Y%m%d%H%M)"
DOCKER_BRANCH_TAG="$DOCKER_REGISTRY/truyo-io:$BITBUCKET_BRANCH"

echo DOCKER_BRANCH_TAG = $DOCKER_BRANCH_TAG
docker build --rm -t $DOCKER_BRANCH_TAG .

if [[ $BITBUCKET_BRANCH == 'development' ]] || [[ $BITBUCKET_BRANCH == 'staging' ]] || [[ $BITBUCKET_BRANCH == 'qa' ]] || [[ $BITBUCKET_BRANCH == 'master' ]]; then

	echo pushing $DOCKER_BRANCH_TAG

    docker push $DOCKER_BRANCH_TAG

    if [[ $BITBUCKET_COMMIT ]]; then

        docker tag $DOCKER_BRANCH_TAG $DOCKER_COMMIT_TAG
        echo pushing $DOCKER_COMMIT_TAG
        docker push $DOCKER_COMMIT_TAG
    fi;
fi
