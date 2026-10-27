FROM mcr.microsoft.com/playwright:v1.49.1-jammy@sha256:6a49e49bafe85e1186e0ce974b854d1fd6964437c969802de6ba88c95c5787a5

LABEL org.opencontainers.image.title="servarr-bootstrap" \
      org.opencontainers.image.description="UMKIM servarr stack bootstrap automation" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/rborso/umkim-thin"

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json ./
COPY LICENSE NOTICE SOURCE.md /usr/share/licenses/servarr-bootstrap/
COPY src ./src
RUN npm run build

ENV NODE_ENV=production
CMD ["node", "dist/main.js"]
