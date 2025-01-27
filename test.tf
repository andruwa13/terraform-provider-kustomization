terraform {
  required_providers {
    kustomization = {
      source = "registry.terraform.io/kbst/kustomization"
      # all test versions are placed as 1.0.0
      # in .terraform/plugins for tests
      version = "0.8.1-rc1"
    }
  }
  required_version = ">= 0.13"
}

provider "kustomization" {
  kubeconfig_path = "~/.kube/config"
}

data "kustomization_build" "test" {
  path = "kustomize/test_kustomizations/basic/initial"
}

resource "kustomization_resource" "from_build" {
  for_each = data.kustomization_build.test.ids

  manifest = data.kustomization_build.test.manifests[each.value]
}

data "kustomization_overlay" "test" {
  namespace = "test-overlay"

  resources = [
    "kustomize/test_kustomizations/basic/initial"
  ]

  patches {
    patch = <<-EOF
      - op: add
        path: /spec/template/spec/containers/0/env
        value: [{"name": "TEST", "value": "true"}]
    EOF

    target {
      group   = "apps"
      version = "v1"
      kind    = "Deployment"
      name    = "test"
    }
  }
}

resource "random_password" "password" {
  length = 16
  special = true
  override_special = "_%@"
}

data "kustomization_overlay" "example" {
  secret_generator {
    name = "example-secret1"
    type = "Opaque"
    literals = [
      "password=${random_password.password.result}",
    ]

    options {
      disable_name_suffix_hash = true
    }
  }

  secret_generator {
    name = "example-secret2"
    literals = [
      "KEY1=VALUE1",
      "KEY2=VALUE2"
    ]
    envs = [
      "path/to/properties.env"
    ]
    files = [
      "path/to/config/file.cfg"
    ]
  }
}

resource "kustomization_resource" "from_overlay" {
  for_each = data.kustomization_overlay.test.ids

  manifest = data.kustomization_overlay.test.manifests[each.value]
}
