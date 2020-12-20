---
title: Teleport Kubernetes Access Guide to Authentication
description: Connect SAML and OIDC users to Kubernetes RBAC with Teleport
---

# Single-Sign-On and Kubernetes RBAC

Teleport issues short lived X.509 certs and updates Kubernetes clients to talk to Teleport proxy using mutual TLS.
It then intercepts every request and adds [impersonation headers](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#user-impersonation)
to map users to Kubernetes users and groups.

![Impersonation](../img/k8s/auth.svg)

=== "Open Source"

    Map Github teams to Kubernetes groups using Github's connector
    `teams_to_logins` section.

    ```yaml
    kind: github
    version: v3
    metadata:
      # connector name that will be used with `tsh --auth=github login`
      name: github
    spec:
      # client ID of Github OAuth app
      client_id: client-id
      # client secret of Github OAuth app
      client_secret: client-secret
      # This name will be shown on UI login screen
      display: Github
      # Change tele.example.com to your domain name
      redirect_url: https://tele.example.com:443/v1/webapi/github/callback
      # Map github teams to kubernetes groups
      teams_to_logins:
        - organization: octocats # Github organization name
          team: admin           # Github team name within that organization
          # list of Kubernetes groups this Github team is allowed to connect to
          kubernetes_groups: ["system:masters"]
          # keep this field as is for now
          logins: ["{% raw %}{{external.username}}{% endraw %}"]
    ```

    Map local user to Kubernetes user and group with `--k8s-groups` and `--k8s-users` flags:
    ``` bash
    # Adding a Teleport local user to map to a Kubernetes user 'joe@kubernetes' and group 'system:masters'
    $ tctl users add joe --k8s-groups="system:masters" --k8s-users="joe@kubernetes"
    ```

=== "Enterprise"
    OIDC and SAML connectors have `claims_to_roles` and `attributes_to_roles` sections to map
    attribute statements or claims received from identity providers to Teleport's roles.

    ```yaml
    kind: saml
    version: v2
    metadata:
      name: okta
    spec:
      acs: https://tele.example.com/v1/webapi/saml/acs
      attributes_to_roles:
      - {name: "groups", value: "okta-admin", roles: ["admin"]}
      entity_descriptor: |
        <?xml !!! Make sure to shift all lines in XML descriptor 
        with 4 spaces, otherwise things will not work
    ```
        
    ```yaml
    # NOTE: the role definition is edited to remove the unnecessary fields
    kind: role
    version: v3
    metadata:
      name: admin
    spec:
      allow:
        # if kubernetes integration is enabled, this setting configures which
        # kubernetes groups the users of this role will be assigned to.
        # note that you can refer to a SAML/OIDC trait via the "external" property bag,
        # this allows you to specify Kubernetes group membership in an identity manager:
        kubernetes_groups: ["system:masters", "{% raw %}{{external.trait_name}}{% endraw %}"]
    ```

It is only necessary to set Kubernetes groups.
If a Kubernetes user is not set the user will impersonate themselves.

### Kubernetes Labels

Labels can be applied to Kubernetes clusters to provide a better inventory of clusters
and more fined grained RBAC.

```yaml
    # ... Snippet of teleport.yaml
    # Optional labels: These can be used in combination with RBAC rules
    # to limit access to applications.
    # When using kubeconfig_file above, these labels apply to all kubernetes
    # clusters specified in the kubeconfig.
    labels:
      env: "prod"
    # Optional Dynamic Labels
    - name: "os"
       command: ["/usr/bin/uname"]
       period: "5s"
    # Get cluster name on GKE.
    - name: cluster-name
      command: ['curl', 'http://metadata.google.internal/computeMetadata/v1/instance/attributes/cluster-name', '-H', 'Metadata-Flavor: Google']
      period: 1m0s
```

## Impersonation

If Teleport is running inside the cluster using a Kubernetes `ServiceAccount`,
here's an example of the permissions that the `ServiceAccount` will need to be able
to use impersonation (change `teleport-serviceaccount` to the name of the `ServiceAccount`
that's being used):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: teleport-impersonation
rules:
- apiGroups:
  - ""
  resources:
  - users
  - groups
  - serviceaccounts
  verbs:
  - impersonate
- apiGroups:
  - ""
  resources:
  - pods
  verbs:
  - get
- apiGroups:
  - "authorization.k8s.io"
  resources:
  - selfsubjectaccessreviews
  verbs:
  - create
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: teleport
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: teleport-impersonation
subjects:
- kind: ServiceAccount
  # this should be changed to the name of the Kubernetes ServiceAccount being used
  name: teleport-serviceaccount
  namespace: default
```

There is also an [example of this usage](https://github.com/gravitational/teleport/blob/master/examples/chart/teleport/templates/clusterrole.yaml)
within the [example Teleport Helm chart](https://github.com/gravitational/teleport/blob/master/examples/chart/teleport/).

If Teleport is running outside of the Kubernetes cluster, you will need to ensure
that the principal used to connect to Kubernetes via the `kubeconfig` file has the
same impersonation permissions as are described in the `ClusterRole` above.

## Kubernetes RBAC

Once you perform the steps above, your Teleport instance should become a fully
functional Kubernetes API proxy. The next step is to configure Teleport to assign
the correct Kubernetes groups to Teleport users.

Mapping Kubernetes groups to Teleport users depends on how Teleport is
configured. In this guide we'll look at two common configurations:

* **Open source, Teleport Community edition** configured to authenticate users via [Github](admin-guide.md#github-oauth-20).
  In this case, we'll need to map Github teams to Kubernetes groups.

* **Commercial, Teleport Enterprise edition** configured to authenticate users via [Okta SSO](enterprise/sso/ssh-okta.md).
  In this case, we'll need to map users' groups that come from Okta to Kubernetes
  groups.


### Github Auth

When configuring Teleport to authenticate against Github, you have to create a
Teleport connector for Github, like the one shown below. Notice the `kubernetes_groups`
setting which assigns Kubernetes groups to a given Github team:

```yaml
kind: github
version: v3
metadata:
  # connector name that will be used with `tsh --auth=github login`
  name: github
spec:
  # client ID of Github OAuth app
  client_id: <client-id>
  # client secret of Github OAuth app
  client_secret: <client-secret>
  # connector display name that will be shown on web UI login screen
  display: Github
  # callback URL that will be called after successful authentication
  redirect_url: https://teleport.example.com:3080/v1/webapi/github/callback
  # mapping of org/team memberships onto allowed logins and roles
  teams_to_logins:
    - organization: octocats # Github organization name
      team: admin           # Github team name within that organization
      # allowed UNIX logins for team octocats/admin:
      logins:
        - root
      # list of Kubernetes groups this Github team is allowed to connect to
      kubernetes_groups: ["system:masters"]
      # Optional: If not set, users will impersonate themselves.
      # kubernetes_users: ['barent']
```

To obtain client ID and client secret from Github, please follow [Github documentation](https://developer.github.com/apps/building-oauth-apps/creating-an-oauth-app/) on how to create and register an OAuth
app. Be sure to set the "Authorization callback URL" to the same value as `redirect_url`
in the resource spec.

Finally, create the Github connector with the command: `tctl create -f github.yaml`.
Now, when Teleport users execute the Teleport's `tsh login` command, they will be
prompted to login through the Github SSO and upon successful authentication, they
have access to Kubernetes.

```bsh
# Login via Github SSO and retrieve SSH+Kubernetes certificates:
$ tsh login --proxy=teleport.example.com --auth=github login

# Use Kubernetes API!
$ kubectl exec -ti <pod-name>
```

The `kubectl exec` request will be routed through the Teleport proxy and
Teleport will log the audit record and record the session.

!!! note

    For more information on integrating Teleport with Github SSO, please see the
    [Github section in the Admin Manual](admin-guide.md#github-oauth-20).

### Okta Auth

With Okta (or any other SAML/OIDC/Active Directory provider), you must update
Teleport's roles to include the mapping to Kubernetes groups.

Let's assume you have the Teleport role called "admin". Add `kubernetes_groups`
setting to it as shown below:


To add `kubernetes_groups` setting to an existing Teleport role, you can either
use the Web UI or `tctl`:

```bsh
# Dump the "admin" role into a file:
$ tctl get roles/admin > admin.yaml
# Edit the file, add kubernetes_groups setting
# and then execute:
$ tctl create -f admin.yaml
```

!!! tip "Advanced Usage"

    `{% raw %}{{ external.trait_name }}{% endraw %}` example is shown to demonstrate how to fetch
    the Kubernetes groups dynamically from Okta during login. In this case, you
    need to define Kubernetes group membership in Okta (as a trait) and use
    that trait name in the Teleport role.

    Teleport 4.3 has an option to extract the local part from an email claim. This can be helpful
    since some operating systems don't support the @ symbol. This means by using `logins: ['{% raw %}{{email.local(external.email)}}{% endraw %}']` the resulting output will be `dave.smith` if the email was dave.smith@acme.com.

Once setup is complete, when users execute `tsh login` and go through the usual Okta login
sequence, their `kubeconfig` will be updated with their Kubernetes credentials.

!!! note

    For more information on integrating Teleport with Okta, please see the
    [Okta integration guide](enterprise/sso/ssh-okta.md).
