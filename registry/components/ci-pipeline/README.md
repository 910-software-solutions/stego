# ci-pipeline

The pinned CI pipeline of each provider. This component emits no code into a service. It states, for each CI
provider, the shared pipeline reference, the STEGO checks reference and the one trusted signer, each pinned by a
full commit or a sha256 digest. `stego apply` writes the service's CI file from these pins, for the provider that
the `delivery.ci` field of the declaration names.

The shipped component states no provider yet. Until the platform owner gives the real values, `stego apply` writes
no CI file and says so in the plan.
