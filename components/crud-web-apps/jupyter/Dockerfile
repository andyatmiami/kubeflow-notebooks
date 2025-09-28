# --- Build the backend kubeflow-wheel ---
FROM python:3.10-slim AS backend-kubeflow-wheel

WORKDIR /src

COPY ./common/backend/ .
RUN python3 setup.py bdist_wheel

# --- Build the frontend kubeflow library ---
FROM node:16.20.2-bullseye AS frontend-kubeflow-lib

WORKDIR /src

COPY ./common/frontend/kubeflow-common-lib/package.json \
     ./common/frontend/kubeflow-common-lib/package-lock.json ./
     
ENV NG_CLI_ANALYTICS="ci"
RUN npm ci

# Copy remaining source files and build in one layer
COPY ./common/frontend/kubeflow-common-lib/projects ./projects
COPY ./common/frontend/kubeflow-common-lib/angular.json \
     ./common/frontend/kubeflow-common-lib/tsconfig.json ./
RUN npm run build

# --- Build the frontend ---
FROM node:16.20.2-bullseye AS frontend

WORKDIR /src

# Copy package files first for better caching
COPY ./jupyter/frontend/package.json \
     ./jupyter/frontend/package-lock.json \
     ./jupyter/frontend/tsconfig.json \
     ./jupyter/frontend/tsconfig.app.json \
     ./jupyter/frontend/tsconfig.spec.json \
     ./jupyter/frontend/angular.json ./
COPY ./jupyter/frontend/src ./src

ENV NG_CLI_ANALYTICS="ci"
RUN npm ci
COPY --from=frontend-kubeflow-lib /src/dist/kubeflow/ ./node_modules/kubeflow/
RUN npm run build -- --output-path=./dist/default --configuration=production

# Web App
FROM python:3.10-slim

WORKDIR /package
COPY --from=backend-kubeflow-wheel /src .
RUN pip3 install .

WORKDIR /src
COPY ./jupyter/backend/requirements.txt .
RUN pip3 install -r requirements.txt

COPY ./jupyter/backend/apps/ ./apps
COPY ./jupyter/backend/entrypoint.py .
COPY --from=frontend /src/dist/default/ /src/apps/default/static/

ENTRYPOINT ["/bin/bash","-c","gunicorn -w 3 --bind 0.0.0.0:5000 --access-logfile - entrypoint:app"]
  
