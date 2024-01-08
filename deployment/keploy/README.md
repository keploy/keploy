# Keploy Helm Chart
The Keploy Helm chart helps easy installation of Keploy on your Kubernetes cluster. It automatically deploys a mongo instance using the [Bitnami Mongo Helm chart](https://github.com/bitnami/charts/tree/master/bitnami/mongodb)   

## Installation
```shell
helm upgrade -i keploy .
```

## Access via kube proxy
```shell
export POD_NAME=$(kubectl get pods --namespace default -l "app.kubernetes.io/name=keploy,app.kubernetes.io/instance=keploy" -o jsonpath="{.items[0].metadata.name}")
export CONTAINER_PORT=$(kubectl get pod --namespace default $POD_NAME -o jsonpath="{.spec.containers[0].ports[0].containerPort}")
kubectl --namespace default port-forward $POD_NAME 8080:$CONTAINER_PORT
```
Then the keploy service should be accessible on http://127.0.0.1:8080

## Access via ingress
To access Keploy though ingress, please add information about ingress in the [values.yaml](values.yaml) file.
 
To host keploy on a subpath of a domain (eg: example.com/keploy). You can set the env value KEPLOY_PATH_PREFIX in [values.yaml] and then build a custom docker image with the KEPLOY_PATH_PREFIX argument set. Then you have to use this docker image in your helm chart.
