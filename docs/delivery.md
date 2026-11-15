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

## The CI file

When the declaration has `delivery.ci`, `stego apply` writes the CI file from the pins of the registry's
`ci-pipeline` component. The shipped component states no provider yet, so `stego apply` writes no CI file and says
so in the plan.

- `github-actions` (`.github/workflows/stego.yml`): the `check` job runs on a pull request, with read permission to
  the contents only, and never pushes or signs. The `release` job runs only on a push to the repository's default
  branch or on a tag, and it pushes and signs the image.
- `konflux` (`.tekton/<service>-push.yaml`): one PipelineRun on a push to the branch `main`. The branch is fixed
  for now. A repository whose default branch is not `main` gets no Konflux run on push.
