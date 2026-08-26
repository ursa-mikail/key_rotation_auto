FROM hashicorp/terraform:1.9

WORKDIR /work
COPY main.tf ./

# Baked in at image build time so the container needs no registry
# access at runtime -- only your machine's Docker build step needs
# internet access to fetch the local provider once.
RUN terraform init

COPY apply-loop.sh /usr/local/bin/apply-loop.sh
RUN chmod +x /usr/local/bin/apply-loop.sh

ENTRYPOINT ["/usr/local/bin/apply-loop.sh"]
