# Deliver a service with Argo CD

`stego apply` writes the files that deliver a service. It writes no value that differs per environment: the
platform supplies those values.

## What STEGO writes

- `deploy/base/`: a plain kustomize base. Its image is `stego.invalid/<service>:set-by-overlay`, which no registry
  serves.
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
