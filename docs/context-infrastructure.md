# Context infrastructure

Agentic applications need more than an LLM context window. They work with
checked-out repositories, intermediate files, durable observations, reference
material, and generated results. Rosso calls the infrastructure that provisions
and attaches those resources **context infrastructure**.

The `context` name is intentional. It describes information and working state
made available to an agent, rather than only the storage mechanism used to hold
it. It is distinct from:

- a **rossoctl configuration context**, which selects an API server, namespace,
  and credential and is managed with `rossoctl config`; and
- an **LLM context window**, which is the bounded prompt and conversation sent
  to a model for one inference.

## Resource model

A named context resource records semantic intent separately from storage
configuration:

| Type | Intended role | Current implementation |
| --- | --- | --- |
| `workspace` | Mutable files used while an agent works | PVC |
| `memory` | Durable observations and experiences | PVC |
| `knowledge` | Synthesized, reusable understanding | PVC |
| `artifacts` | Reports, media, and other produced outputs | PVC |

The type is metadata today. It does not yet change provisioning, lifecycle, or
access policy. Keeping it separate from the backend allows future implementations
such as object storage or cached object-backed filesystems without changing how
users describe the role of the resource.

```text
Context Service                 Rosso                         Agent
---------------                 -----                         -----
provisions named storage  -->   resolves the attachment  --> mounts the PVC
returns a PVC attachment        while importing an agent      at the chosen path
```

Contexts have an independent lifecycle. Deleting an agent does not delete its
context, allowing another agent to mount the same resource. Delete it explicitly
when it is no longer needed.

## Examples

Create a private ReadWriteOnce workspace:

```sh
rossoctl context create research --size 5Gi
```

Create a shared ReadWriteMany workspace:

```sh
rossoctl context create shared-research \
  --shared --size 10Gi --storage-class ibm-scale-csi
```

Create resources with other semantic roles:

```sh
rossoctl context create research-memory --type memory --size 5Gi
rossoctl context create research-library --type knowledge --shared --size 20Gi
rossoctl context create research-results --type artifacts --shared --size 20Gi
```

Mount a context into a StatefulSet or Sandbox agent:

```sh
rossoctl agents import --deployment-type statefulset \
  --context research:/workspace \
  from-image --name researcher --containerImage IMAGE

rossoctl agents import --deployment-type sandbox \
  --context shared-research:/workspace \
  from-image --name reviewer --containerImage IMAGE
```

Inspect and remove resources:

```sh
rossoctl context list
rossoctl context get research
rossoctl context delete research
```

## Server compatibility

The CLI commands require the context resource API introduced by
[rossoctl/rossoctl#2392](https://github.com/rossoctl/rossoctl/pull/2392). Until it
appears in a numbered Rosso release, use a server built from a later `main`
commit. `rossoctl context list` identifies the common older-server 404 and
explains that the server must be upgraded instead of returning only the raw
HTTP error.
