FROM hashicorp/terraform:1.9

# curl: what null_resource.trigger_rotation's local-exec provisioner
# uses to call Go's /api/rotate-if-due. jq is NOT needed in this
# variant -- the due-check moved into Terraform's own HCL (timecmp()
# in main.tf), so apply-loop.sh no longer parses the tfvars json
# itself; it just calls `terraform apply` on a timer.
RUN apk add --no-cache curl

WORKDIR /work
COPY main.tf ./

# Baked in at image build time so the container needs no registry
# access at runtime -- only your machine's Docker build step needs
# internet access to fetch the local and null providers once.
#
# `terraform validate` here means an HCL error in main.tf (a typo, a
# type mismatch, a bad reference) fails the DOCKER BUILD itself with a
# clear message, instead of silently producing a working-looking image
# that then fails every single `terraform apply` at runtime with the
# failure only visible in `docker compose logs terraform-runner`.
RUN terraform init && terraform validate

COPY apply-loop.sh /usr/local/bin/apply-loop.sh
RUN chmod +x /usr/local/bin/apply-loop.sh

ENTRYPOINT ["/usr/local/bin/apply-loop.sh"]
