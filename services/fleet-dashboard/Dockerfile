# syntax=docker/dockerfile:1.7
# Build from the repository root: docker build -f services/fleet-dashboard/Dockerfile .

FROM node:22-bookworm-slim AS deps
WORKDIR /app
COPY services/fleet-dashboard/package.json services/fleet-dashboard/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci

FROM node:22-bookworm-slim AS build
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
COPY --from=deps /app/node_modules ./node_modules
COPY services/fleet-dashboard ./
RUN npm run build

# The standalone output holds only the files the server needs, without dev dependencies.
FROM node:22-bookworm-slim
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 PORT=3000 HOSTNAME=0.0.0.0
COPY --from=build --chown=1000:1000 /app/.next/standalone ./
# Numeric, because Kubernetes can only enforce runAsNonRoot when it can see the user ID.
USER 1000
EXPOSE 3000
CMD ["node", "server.js"]
