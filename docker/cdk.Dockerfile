FROM node:20-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends docker.io ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g aws-cdk
WORKDIR /work/cdk
