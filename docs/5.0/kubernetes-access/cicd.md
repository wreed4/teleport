# Using Teleport Kubernetes with CI/CD

Teleport can integrate with CI/CD tooling for greater visibility and auditability of
these tools. For this we recommend creating a local Teleport user, then exporting
a kubeconfig using [`tctl auth sign`](cli-docs.md#tctl-auth-sign)

An example setup is below.

```bash
# Create a new local user for Jenkins
$ tctl users add jenkins
# Option 1: Creates a token for 1 year
$ tctl auth sign --user=jenkins --format=kubernetes --out=kubeconfig --ttl=8760h
# Recommended Option 2: Creates a token for 25hrs
$ tctl auth sign --user=jenkins --format=kubernetes --out=kubeconfig --ttl=25h

  The credentials have been written to kubeconfig

$ cat kubeconfig
  apiVersion: v1
  clusters:
  - cluster:
      certificate-authority-data: LS0tLS1CRUdJTiBDRVJUSUZ....
# This kubeconfig can now be exported and will provide access to the automation tooling.

# Uses kubectl to get pods, using the provided kubeconfig.
$ kubectl --kubeconfig /path/to/kubeconfig get pods
```

!!! tip "How long should TTL be?"

    In the above example we've provided two options. One with 1yr (8760h) time to live
    and one for just 25hrs. As proponents of short lived SSH certificates we recommend
    the same for automation.

    Handling secrets is out of scope of our docs, but at a high level we recommend
    using providers secrets managers. Such as [AWS Secrets Manager](https://aws.amazon.com/secrets-manager/),
    [GCP Secrets Manager](https://cloud.google.com/secret-manager), or on prem using
    a project like [Vault](https://www.vaultproject.io/).  Then running a nightly
    job on the auth server to sign and publish a new kubeconfig. In our example, we've
    added 1hr, and during this time both kubeconfigs will be valid.

    Taking this a step further you could build a system to request a very short lived
    token for each CI run. We plan to make this easier for operators to integrate in
    the future by exposing and documenting more of our API.
