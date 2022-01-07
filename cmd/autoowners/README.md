# autoowners

`autoowners` updates local `OWNERS` files from remote GitHub repositories.

## What it does

Given any repository structure that follows the `org/repo` naming convention (such as
the [one storing ci-operator configuration in openshift/release](https://github.com/openshift/release/tree/master/ci-operator/config))
, the `autoowners` utility fetches the `OWNERS` file from each `org/repo` repository in GitHub and updates the
local `OWNERS` file in the directory structure.

# OLD CONTENT BELOW 

Upstream repositories are calculated from `{target-subdir}/{config-subdir[0]}/{organization}/{repository}`. For example,
given  `... -target-subdir=ci-operator -config-subdir=jobs,... ...` the presence
of [`ci-operator/jobs/openshift/origin`][openshift/origin-jobs] inserts [openshift/origin][] as an upstream repository.

The `HEAD` branch for each upstream repository is pulled to extract its `OWNERS` and `OWNERS_ALIASES`. If `OWNERS` is
missing, the utility will ignore `OWNERS_ALIASES`, even if it is present upstream.

Any aliases present in the upstream `OWNERS` file will be resolved to the set of usernames they represent in the
associated
`OWNERS_ALIASES` file. The local `OWNERS` files will therefore not contain any alias names. This avoids any conflicts
between upstream alias names coming from different repos.

The utility also iterates through the `{target-subdir}/{type}/{organization}/{repository}` for `{type}` in `config`
, `jobs`, and `templates`, writing `OWNERS` to reflect the upstream configuration. If the upstream does not have
an `OWNERS` file, the utility will ignore syncing it for those paths.

Test it locally with existing image:

```console
$ git clone https://github.com/openshift/release /tmp/release
$ cd /tmp/release
$ podman run --entrypoint "/bin/bash" -v "${PWD}:/tmp/release":z --workdir /tmp/release -it --rm registry.ci.openshift.org/ci/autoowners
$ mkdir /etc/github
$ echo github_token > /etc/github/oauth
$ /usr/bin/autoowners --github-token-path=/etc/github/oauth --git-name=openshift-bot --git-email=openshift-bot@redhat.com --target-dir=. --ignore-repo="ci-operator/config/openshift/kubernetes-metrics-server" --ignore-repo="ci-operator/jobs/openshift/kubernetes-metrics-server" --ignore-repo="ci-operator/config/openshift/origin-metrics" --ignore-repo="ci-operator/jobs/openshift/origin-metrics" --ignore-repo="ci-operator/config/openshift/origin-web-console" --ignore-repo="ci-operator/jobs/openshift/origin-web-console" --ignore-repo="ci-operator/config/openshift/origin-web-console-server" --ignore-repo="ci-operator/jobs/openshift/origin-web-console-server" --ignore-repo="ci-operator/jobs/openvswitch/ovn-kubernetes" --ignore-repo="ci-operator/config/openshift/cluster-api-provider-azure" --ignore-repo="ci-operator/config/openshift/csi-driver-registrar" --ignore-repo="ci-operator/config/openshift/csi-external-resizer" --ignore-repo="ci-operator/config/openshift/csi-external-snapshotter" --ignore-repo="ci-operator/config/openshift/csi-livenessprobe" --ignore-repo="ci-operator/config/openshift/knative-build" --ignore-repo="ci-operator/config/openshift/knative-client" --ignore-repo="ci-operator/config/openshift/knative-serving" --ignore-repo="ci-operator/config/openshift/kubernetes" --ignore-repo="ci-operator/config/openshift/sig-storage-local-static-provisioner" --ignore-repo="ci-operator/jobs/openshift/cluster-api-provider-azure" --ignore-repo="ci-operator/jobs/openshift/csi-driver-registrar" --ignore-repo="ci-operator/jobs/openshift/csi-external-resizer" --ignore-repo="ci-operator/jobs/openshift/csi-external-snapshotter" --ignore-repo="ci-operator/jobs/openshift/csi-livenessprobe" --ignore-repo="ci-operator/jobs/openshift/knative-build" --ignore-repo="ci-operator/jobs/openshift/knative-client" --ignore-repo="ci-operator/jobs/openshift/knative-serving" --ignore-repo="ci-operator/jobs/openshift/kubernetes" --ignore-repo="ci-operator/jobs/openshift/sig-storage-local-static-provisioner"
```

[openshift/origin]: https://github.com/openshift/origin

[openshift/origin-jobs]: https://github.com/openshift/release/tree/master/ci-operator/jobs/openshift/origin
