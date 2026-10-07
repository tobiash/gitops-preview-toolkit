package fmp

import rego.v1

enabled(id) if id in input.builtins

logical_identity(change) := {"logicalId": id |
  id := object.get(change, "logicalId", "")
  id != ""
}

network_kind(kind) if kind == "Ingress"
network_kind(kind) if kind == "Gateway"
network_kind(kind) if kind == "HTTPRoute"

stateful_kind(kind) if kind == "StatefulSet"
stateful_kind(kind) if kind == "DaemonSet"

replica_kind(kind) if kind == "Deployment"
replica_kind(kind) if kind == "StatefulSet"
replica_kind(kind) if kind == "ReplicaSet"

classifications contains object.union({
  "id": "image_update",
  "priority": 20,
  "title": "Container image updated",
  "severity": "info",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": sprintf("%s image changed", [change.kind])
}, logical_identity(change)) if {
  enabled("image_update")
  some change in input.changes
  change.action == "modified"
  some i
  old_image := change.old.spec.template.spec.containers[i].image
  new_image := change.new.spec.template.spec.containers[i].image
  old_image != new_image
}

classifications contains object.union({
  "id": "secret_change",
  "priority": 80,
  "title": "Secret changed",
  "severity": "warning",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": "Secret data changed"
}, logical_identity(change)) if {
  enabled("secret_change")
  some change in input.changes
  change.kind == "Secret"
}

classifications contains object.union({
  "id": "ingress_change",
  "priority": 50,
  "title": "Networking changed",
  "severity": "warning",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": sprintf("%s routing changed", [change.kind])
}, logical_identity(change)) if {
  enabled("ingress_change")
  some change in input.changes
  network_kind(change.kind)
}

classifications contains object.union({
  "id": "crd_change",
  "priority": 90,
  "title": "CRD changed",
  "severity": "warning",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": "CustomResourceDefinition changed"
}, logical_identity(change)) if {
  enabled("crd_change")
  some change in input.changes
  change.kind == "CustomResourceDefinition"
}

classifications contains object.union({
  "id": "namespace_delete",
  "priority": 100,
  "title": "Namespace removed",
  "severity": "error",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": "Namespace deleted"
}, logical_identity(change)) if {
  enabled("namespace_delete")
  some change in input.changes
  change.action == "deleted"
  change.kind == "Namespace"
}

classifications contains object.union({
  "id": "stateful_workload_change",
  "priority": 40,
  "title": "Stateful workload changed",
  "severity": "warning",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": sprintf("%s changed", [change.kind])
}, logical_identity(change)) if {
  enabled("stateful_workload_change")
  some change in input.changes
  stateful_kind(change.kind)
}

classifications contains object.union({
  "id": "pvc_change",
  "priority": 70,
  "title": "Persistent volume claim changed",
  "severity": "warning",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": "PersistentVolumeClaim changed"
}, logical_identity(change)) if {
  enabled("pvc_change")
  some change in input.changes
  change.kind == "PersistentVolumeClaim"
}

classifications contains object.union({
  "id": "service_type_change",
  "priority": 60,
  "title": "Service type changed",
  "severity": "warning",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": sprintf("Service type changed to %v", [change.new.spec.type])
}, logical_identity(change)) if {
  enabled("service_type_change")
  some change in input.changes
  change.action == "modified"
  change.kind == "Service"
  old_spec := object.get(change.old, "spec", {})
  new_spec := object.get(change.new, "spec", {})
  old_type := object.get(old_spec, "type", "ClusterIP")
  new_type := object.get(new_spec, "type", "ClusterIP")
  old_type != new_type
}

classifications contains object.union({
  "id": "replicas_change",
  "priority": 30,
  "title": "Replica count changed",
  "severity": "info",
  "kind": change.kind,
  "namespace": change.namespace,
  "name": change.name,
  "cluster": object.get(change, "cluster", ""),
  "message": sprintf("Replicas changed from %v to %v", [old_replicas, new_replicas])
}, logical_identity(change)) if {
  enabled("replicas_change")
  some change in input.changes
  change.action == "modified"
  replica_kind(change.kind)
  old_spec := object.get(change.old, "spec", {})
  new_spec := object.get(change.new, "spec", {})
  old_replicas := object.get(old_spec, "replicas", 1)
  new_replicas := object.get(new_spec, "replicas", 1)
  old_replicas != new_replicas
}
