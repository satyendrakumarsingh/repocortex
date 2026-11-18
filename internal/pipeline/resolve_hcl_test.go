package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestHCLResolution indexes a small Terraform repository (a root module, a child
// module, a sibling directory that declares the same names, and a Packer template) and
// checks every reference against what Terraform's own scoping makes it reach.
//
// What it is built to catch: `var.region` in outputs.tf, which is declared in
// variables.tf (every `.tf` file of a directory is one module) and not in other/, whose
// `variable "region"` and `aws_instance.web` have the same address; `module.net.subnet_id`,
// written in a file other than the one that declares `module "net"`, which is the
// `output "subnet_id"` of modules/net and not of any other module; `module.vpc.vpc_id`,
// of a registry module, which is outside the repository; `module.net` and
// `data.aws_ami.ubuntu`; `provider = aws.west`; a `region = "x"` in terraform.tfvars,
// which sets the variable of its own directory; `each.value`, `count.index`, `self.id` and
// `path.module`, which are no address; and a Packer template's `var.x`, which is not
// Terraform's.
//
// What it does not bind, by design: a reference to a block the directory does not declare
// (a resource of another module), and anything written in a `.hcl` file.
func TestHCLResolution(t *testing.T) {
	got := langEdges(t, hclFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// A block of the same directory, in whichever file it is written.
		{"main.tf:local.prefix", "variables.tf:var.region", high},
		{"main.tf:local.tags", "main.tf:local.prefix", high},
		{"main.tf:module.net", "variables.tf:var.cidr", high},
		{"main.tf:aws_instance.web", "main.tf:data.aws_ami.ubuntu", high},
		{"main.tf:aws_instance.web", "main.tf:local.tags", high},
		{"main.tf:aws_instance.web", "main.tf:aws_security_group.web", high},
		{"main.tf:aws_instance.web", "main.tf:provider.aws.west", high},
		{"main.tf:aws_instance.web", "main.tf:module.net", high},
		{"outputs.tf:output.ip", "main.tf:aws_instance.web", high},
		{"outputs.tf:output.region", "variables.tf:var.region", high},
		{"modules/net/main.tf:aws_subnet.this", "modules/net/variables.tf:var.cidr", high},
		{"modules/net/main.tf:output.subnet_id", "modules/net/main.tf:aws_subnet.this", high},
		// The output of the module a `module` block names, from another file of the directory.
		{"main.tf:aws_instance.web", "modules/net/main.tf:output.subnet_id", high},
		{"outputs.tf:output.subnet", "modules/net/main.tf:output.subnet_id", high},
		// A .tfvars sets the variable of its own directory.
		{"terraform.tfvars:(top level)", "variables.tf:var.region", high},
		// A module block of a registry module, and the provider block's own region.
		{"main.tf:aws_eip.ip", "main.tf:module.vpc", high},
		{"main.tf:provider.aws", "variables.tf:var.region", high},
		{"outputs.tf:output.subnet", "main.tf:module.net", high},
		// The sibling module, which has the same addresses, resolves in its own directory.
		{"other/main.tf:aws_instance.web", "other/main.tf:var.region", high},
		{"other/main.tf:output.subnet_id", "other/main.tf:var.cidr", high},
	}
	forbid := []want{
		// other/ declares the same addresses and is another module.
		{Src: "main.tf", Dst: "other/"},
		{Src: "outputs.tf", Dst: "other/"},
		{Src: "modules/net/", Dst: "other/"},
		{Src: "terraform.tfvars", Dst: "other/"},
		// A module's output is not the root's variable of the same name.
		{Src: "outputs.tf:output.subnet", Dst: "variables.tf"},
		// A Packer template is not Terraform.
		{Src: "packer/", Dst: ""},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func hclFixture() map[string]string {
	return map[string]string{
		"main.tf": `provider "aws" {
  region = var.region
}

provider "aws" {
  alias = "west"
}

locals {
  prefix = "${var.region}-app"
  tags   = merge({ Name = local.prefix })
}

module "net" {
  source = "./modules/net"
  cidr   = var.cidr
}

module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
}

data "aws_ami" "ubuntu" {
  most_recent = true
}

resource "aws_security_group" "web" {
  name = "web"
}

resource "aws_instance" "web" {
  ami                    = data.aws_ami.ubuntu.id
  subnet_id              = module.net.subnet_id
  tags                   = local.tags
  vpc_security_group_ids = [aws_security_group.web.id]
  provider               = aws.west
  depends_on             = [module.net]

  dynamic "ebs" {
    for_each = [1]
    content {
      size = each.value
      name = "${count.index}-${self.id}-${path.module}"
    }
  }
}

resource "aws_eip" "ip" {
  vpc_id = module.vpc.vpc_id
}
`,
		"outputs.tf": `output "ip" {
  value = aws_instance.web.private_ip
}

output "subnet" {
  value = module.net.subnet_id
}

output "region" {
  value = var.region
}
`,
		"variables.tf": `variable "region" {
  type = string
}

variable "cidr" {
  type = string
}
`,
		"terraform.tfvars": `region = "us-east-1"
`,
		"modules/net/main.tf": `resource "aws_subnet" "this" {
  cidr_block = var.cidr
}

output "subnet_id" {
  value = aws_subnet.this.id
}
`,
		"modules/net/variables.tf": `variable "cidr" {
  type = string
}
`,
		// The same addresses as the root module, in a module nobody names.
		"other/main.tf": `variable "region" {
  type = string
}

variable "cidr" {
  type = string
}

resource "aws_instance" "web" {
  ami = var.region
}

output "subnet_id" {
  value = var.cidr
}
`,
		"packer/build.pkr.hcl": `variable "x" {
  default = "a"
}

source "amazon-ebs" "base" {
  ami_name = var.x
}
`,
	}
}
