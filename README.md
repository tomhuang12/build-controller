# build-controller

The build-controller is a Kubernetes operator that experiments turning continuous integration build process from event-driven to GitOps driven. It integrates with Flux using the GitOps Toolkit and enables building GitOps-based CI pipelines.

Currently, it supports `DockerBuild` that performs `docker build` and `docker push` on a given repository and path that contains a `Dockerfile`. 

## Getting Started

The build-controller requires that you already have the [GitOps toolkit](https://fluxcd.io/docs/components/)
controllers installed in your cluster. Visit [https://fluxcd.io/docs/get-started/](https://fluxcd.io/docs/get-started/) for information on getting started if you are new to `flux`.

### Installation

{% note %}

**Note:** Running the controller locally requires docker to be running.

{% endnote %}

Install flux on your test cluster:

```bash
flux install
```

Port forward to source-controller artifacts server:

```bash
kubectl -n flux-system port-forward svc/source-controller 8080:80
```

Export the local address as `SOURCE_CONTROLLER_LOCALHOST`:

```bash
export SOURCE_CONTROLLER_LOCALHOST=localhost:8080
```

Run the controller locally:

```
make install
make run
```

### Quick Start
#### Create a dockerhub repository

Create a dockerhub repository `build-controller-example` using your dockerhub account

#### Define a Git repository source

Create a source object that points to a Git repository containing application and a `Dockerfile`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1beta1
kind: GitRepository
metadata:
  name: example
  namespace: default
spec:
  interval: 5m
  url: https://github.com/tomhuang12/build-controller-example
  ref:
    branch: main
```

#### Create a Docker Hub authentication secret

Create a docker hub authentication secret containing your dockerhub username, password or access token, and dockerhub server address.

```bash
kubectl create secret generic docker-auth-config \
--from-literal=Username=username \
--from-literal=Password=password \
--from-literal=ServerAddress="https://index.docker.io/v1/"
```

Output:
```yaml
apiVersion: v1
data:
  Password: cGFzc3dvcmQ=
  ServerAddress: aHR0cHM6Ly9pbmRleC5kb2NrZXIuaW8vdjEv
  Username: dXNlcm5hbWU=
kind: Secret
metadata:
  name: docker-auth-config
  namespace: default
```

#### Define a DockerBuild

Create a `DockerBuild` resource that references the `GitRepository` source previously defined.

```yaml
apiVersion: build.contrib.flux.io/v1alpha1
kind: DockerBuild
metadata:
  name: example
  namespace: default
spec:
  interval: 5m
  buildMode: buildPush
  sourceRef:
    kind: GitRepository
    name: example
  containerRegistry:
    repository: <username>/build-controller-example
    tagStrategy: commitSHA
    authConfigRef:
      name: docker-auth-config
      namespace: default
```

### Development

`make test` requires `DOCKERBUILD_USERNAME`, `DOCKERBUILD_PASSWORD`, `DOCKERBUILD_SERVER` to be set as environment variables to actually perform `docker push` to a registry that exists.

```
make DOCKERBUILD_USERNAME="<your-username>" DOCKERBUILD_PASSWORD="<your-token>" DOCKERBUILD_SERVER="<docker-server-address>" test
```