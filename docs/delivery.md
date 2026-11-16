# Deliver a service with Argo CD

`stego apply` writes the files that deliver a service. It writes no value that differs per environment: the
platform supplies those values.

## What STEGO writes

- `deploy/base/`: a plain kustomize base. Its image is `stego.invalid/<service>:set-by-overlay`, which no registry
  serves. The container has one `env` entry for each variable that the service reads. See "Secrets and
  configuration".
- `deploy/components/secret-files/`, when a variable of the service comes from a secret: an opt-in kustomize
  Component that gives each secret as a file. See "Secrets as files".
- `deploy/argocd/applicationset.yaml`, when the declaration has `delivery.argocd`: an Argo CD ApplicationSet. Its git
  files generator reads the platform's environment files. Each Application has one source: the overlay at the
  `destination.overlay_path` of its environment file, in the repository of the environment files.

## What the platform must do

1. Write one overlay for each environment. The overlay must pull the base as a remote git resource, pinned to the
   full 40-character commit of the release. `stego apply` prints the exact line in the plan and writes it in the
   header of the ApplicationSet, for example:

   ```yaml
   resources:
     - https://git.example/orders.git//deploy/base?ref=0123456789abcdef0123456789abcdef01234567
   ```

2. Set the image by digest in each overlay. A pod with the placeholder image does not start.
3. Give each service a locked AppProject: restrict `sourceRepos` and `destinations`, and refuse the namespaces
   `kube-system` and `argocd`. STEGO refuses the project `default`.
4. Choose the revision of the environment files. A full commit pins them. A protected branch follows them.

## Secrets and configuration

The base names each Secret, each ConfigMap and each key. It never holds a value. The platform makes the Secrets
and the ConfigMap with its own tool, for example the External Secrets Operator, Sealed Secrets, SOPS or a kustomize
generator in the overlay. `deploy/platform-requirements.yaml` lists the same names, and `stego env check` checks an
environment file against them.

- A secret variable takes its value from a key of a secret need, for example:

  ```yaml
  - name: DATABASE_URL
    valueFrom:
      secretKeyRef:
        name: orders-database
        key: url
  ```

  The Secret is `<service>-<need>`, and the key is a key of that need. The entry has no `optional` flag, because
  the secret need requires each of its keys.
- Each other variable takes its value from the ConfigMap `<service>-config`, with the variable name as the key. A
  required variable has no `optional` flag. An optional variable has `optional: true`: when the key is not there,
  the service uses the default in its code. The base writes no default.
- A variable with a `*_FILE` form, such as `DATABASE_URL` and `DATABASE_URL_FILE`, gets the environment form only.
  The service refuses the two together.
- A secret variable that no secret need gives, such as `PGPASSWORD`, gets no entry.
- `STEGO_PROBE_CONSOLE` gets no entry, so the domain probes stay off the console. An overlay that wants them on
  the console sets the variable itself.

When a required key is not there, the pod does not start. The kubelet does not start the container, and the pod
stays in `CreateContainerConfigError`. The pod event names the Secret or the ConfigMap and the key, for example
`couldn't find key url in Secret orders-test/orders-database`, or `secret "orders-database" not found`. The kubelet
tries again, and the pod starts when the key is there. The process never runs without a required value. Argo CD
shows the Application as Progressing or Degraded.

A new value of a Secret or of the ConfigMap reaches the process only after a restart of the pod. Use a tool such as
Reloader, or a rollout from your secret operator, if you need that.

When the platform uses other names, the overlay must patch them. A `namePrefix` or a `nameSuffix` of the overlay
does not rename a Secret that the overlay does not make, such as one that an operator makes on the cluster.

## Secrets as files

The Component `deploy/components/secret-files/` gives each secret to the service as a file, in place of an
environment variable. A file does not go to child processes, logs or process dumps, and a new value of the Secret
reaches the file in place. List the Component in the overlay, next to the base, at the same commit:

```yaml
resources:
  - https://git.example/orders.git//deploy/base?ref=0123456789abcdef0123456789abcdef01234567
components:
  - https://git.example/orders.git//deploy/components/secret-files?ref=0123456789abcdef0123456789abcdef01234567
```

The Component has two patches of the Deployment. The forms are exclusive, so the first patch removes the
environment entry of each secret variable, and of each path variable that names the file of a secret. The second
patch then:

- mounts each Secret read-only at `/var/run/secrets/stego/<secret>`, with no `subPath`, and with the mode 0400;
- sets the `*_FILE` variable, or the path variable, to the path of the file of its key, for example
  `DATABASE_URL_FILE=/var/run/secrets/stego/orders-database/url`.

The files have the mode 0400 and the owner root, so that no other user can read them. The process runs as a user
that is not root, so give the pod an `fsGroup`: Kubernetes then gives the files to that group with read access.
The restricted SCC of OpenShift sets an `fsGroup` for each namespace. On another cluster, set it in the overlay.

When a Secret is not there, the pod stays in `ContainerCreating`, and the pod event `FailedMount` names the Secret.
When a key is not there, the service does not start and names the variable of the file.

## The CI file

When the declaration has `delivery.ci`, `stego apply` writes the CI file from the pins of the registry's
`ci-pipeline` component. The shipped component states no provider yet, so `stego apply` writes no CI file and says
so in the plan.

- `github-actions` (`.github/workflows/stego.yml`): the `check` job runs on a pull request, with read permission to
  the contents only, and never pushes or signs. The `release` job runs only on a push to the repository's default
  branch or on a tag, and it pushes and signs the image.
- `konflux` (`.tekton/<service>-push.yaml`): one PipelineRun on a push to the branch `main`. The branch is fixed
  for now. A repository whose default branch is not `main` gets no Konflux run on push.
