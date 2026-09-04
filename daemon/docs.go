package main

import (
	"net/http"
)

func registerDocsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /docs", serveSwagger)
	mux.HandleFunc("GET /swagger", serveSwagger)
	mux.HandleFunc("GET /openapi.json", serveOpenAPI)
}

func serveSwagger(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(swaggerHTML))
}

func serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(openAPIJSON))
}

const swaggerHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Chamber Daemon API</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0; background: #f7f7f7; }
    #swagger-ui { max-width: 1180px; margin: 0 auto; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = function() {
      SwaggerUIBundle({
        url: "/openapi.json",
        dom_id: "#swagger-ui",
        deepLinking: true,
        presets: [SwaggerUIBundle.presets.apis],
      });
    };
  </script>
</body>
</html>
`

const openAPIJSON = `{
  "openapi": "3.1.0",
  "info": {
    "title": "Chamber Daemon API",
    "version": "v1",
    "description": "Local Chamber daemon HTTP API for pulling OCI images, creating, starting, stopping, removing, listing containers, and reading stored logs. The daemon listens on a user-scoped Unix socket by default; TCP/HTTP is an explicit development option."
  },
  "paths": {
    "/healthz": {
      "get": {
        "summary": "Check whether the daemon HTTP server is running",
        "operationId": "health",
        "responses": {
          "200": {
            "description": "HTTP server is running",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "required": ["status"],
                  "properties": {
                    "status": { "type": "string", "example": "ok" }
                  }
                }
              }
            }
          }
        }
      }
    },
    "/v1/images/pull": {
      "post": {
        "summary": "Pull an OCI image into Chamber storage",
        "operationId": "pullImage",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": { "$ref": "#/components/schemas/PullImageRequest" },
              "examples": {
                "alpine": {
                  "value": { "reference": "docker.io/library/alpine:latest" }
                }
              }
            }
          }
        },
        "responses": {
          "200": {
            "description": "Image pulled",
            "headers": {
              "X-Chamber-Operation-ID": {
                "schema": { "type": "string" }
              }
            },
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/PullImageResponse" }
              }
            }
          },
          "409": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" },
          "400": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers": {
      "get": {
        "summary": "List containers recorded by the local daemon",
        "operationId": "listContainers",
        "responses": {
          "200": {
            "description": "Containers listed",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/ListContainersResponse" }
              }
            }
          },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers/create": {
      "post": {
        "summary": "Create a prepared container without starting it",
        "operationId": "createContainer",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": { "$ref": "#/components/schemas/RunContainerRequest" }
            }
          }
        },
        "responses": {
          "201": {
            "description": "Container prepared",
            "headers": {
              "X-Chamber-Operation-ID": {
                "schema": { "type": "string" }
              }
            },
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/RunContainerResponse" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "409": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers/run": {
      "post": {
        "summary": "Create and start a container from a pulled image",
        "operationId": "runContainer",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": { "$ref": "#/components/schemas/RunContainerRequest" },
              "examples": {
                "alpineShell": {
                  "value": {
                    "image": "docker.io/library/alpine:latest",
                    "command": ["/bin/sh", "-c", "id && echo chamber"]
                  }
                },
                "workspaceTest": {
                  "value": {
                    "image": "docker.io/library/golang:latest",
                    "command": ["/bin/sh", "-lc", "cd /workspace && GOCACHE=/tmp/chamber-go-cache go test ./..."],
                    "mounts": [
                      {
                        "type": "bind",
                        "source": "/home/user/chamber",
                        "target": "/workspace",
                        "options": ["rbind", "ro"]
                      }
                    ]
                  }
                }
              }
            }
          }
        },
        "responses": {
          "201": {
            "description": "Container started or completed",
            "headers": {
              "X-Chamber-Operation-ID": {
                "schema": { "type": "string" }
              }
            },
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/RunContainerResponse" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "409": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers/{id}/start": {
      "post": {
        "summary": "Start a prepared container",
        "operationId": "startContainer",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": { "type": "string" }
          }
        ],
        "responses": {
          "200": {
            "description": "Container start initiated",
            "headers": {
              "X-Chamber-Operation-ID": {
                "schema": { "type": "string" }
              }
            },
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Container" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "409": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers/{id}": {
      "get": {
        "summary": "Read one container record",
        "operationId": "getContainer",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": { "type": "string" }
          }
        ],
        "responses": {
          "200": {
            "description": "Container record",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Container" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      },
      "delete": {
        "summary": "Remove a container record and owned artifacts",
        "operationId": "removeContainer",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": { "type": "string" }
          }
        ],
        "responses": {
          "200": {
            "description": "Container removed",
            "headers": {
              "X-Chamber-Operation-ID": {
                "schema": { "type": "string" }
              }
            },
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Container" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "409": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers/{id}/stop": {
      "post": {
        "summary": "Send SIGTERM to a running container without deleting artifacts",
        "operationId": "stopContainer",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": { "type": "string" }
          }
        ],
        "responses": {
          "200": {
            "description": "Stop signal delivered",
            "headers": {
              "X-Chamber-Operation-ID": {
                "schema": { "type": "string" }
              }
            },
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Container" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "409": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers/{id}/cancel": {
      "post": {
        "summary": "Force-cancel a container and clean up owned artifacts",
        "operationId": "cancelContainer",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": { "type": "string" }
          }
        ],
        "responses": {
          "200": {
            "description": "Container canceled",
            "headers": {
              "X-Chamber-Operation-ID": {
                "schema": { "type": "string" }
              }
            },
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Container" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "409": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/containers/{id}/logs": {
      "get": {
        "summary": "Read stored container logs",
        "operationId": "containerLogs",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": { "type": "string" }
          }
        ],
        "responses": {
          "200": {
            "description": "Log content",
            "content": {
              "text/plain": {
                "schema": { "type": "string" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/operations": {
      "get": {
        "summary": "List durable daemon operations and pending cleanup work",
        "operationId": "listOperations",
        "responses": {
          "200": {
            "description": "Operations listed",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/ListOperationsResponse" }
              }
            }
          },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/operations/{id}": {
      "get": {
        "summary": "Read one durable daemon operation",
        "operationId": "getOperation",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": { "type": "string" }
          }
        ],
        "responses": {
          "200": {
            "description": "Operation record",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Operation" }
              }
            }
          },
          "400": { "$ref": "#/components/responses/Error" },
          "404": { "$ref": "#/components/responses/Error" },
          "500": { "$ref": "#/components/responses/Error" }
        }
      }
    },
    "/v1/system/info": {
      "get": {
        "summary": "Read the selected daemon startup validation report",
        "operationId": "systemInfo",
        "responses": {
          "200": {
            "description": "Startup validation report",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/SystemInfoResponse" }
              }
            }
          }
        }
      }
    }
  },
  "components": {
    "responses": {
      "Error": {
        "description": "HTTP error",
        "content": {
          "application/json": {
            "schema": { "$ref": "#/components/schemas/ErrorResponse" }
          }
        }
      }
    },
    "schemas": {
      "PullImageRequest": {
        "type": "object",
        "required": ["reference"],
        "additionalProperties": false,
        "properties": {
          "reference": {
            "type": "string",
            "example": "docker.io/library/alpine:latest"
          }
        }
      },
      "PullImageResponse": {
        "type": "object",
        "required": ["operation_id", "reference", "digest", "pulled_at"],
        "additionalProperties": false,
        "properties": {
          "operation_id": { "type": "string" },
          "reference": { "type": "string" },
          "digest": { "type": "string" },
          "pulled_at": { "type": "string", "format": "date-time" }
        }
      },
      "RunContainerRequest": {
        "type": "object",
        "required": ["image", "command"],
        "additionalProperties": false,
        "properties": {
          "image": {
            "type": "string",
            "example": "docker.io/library/alpine:latest"
          },
          "command": {
            "type": "array",
            "items": { "type": "string" },
            "minItems": 1,
            "example": ["/bin/sh", "-c", "id && echo chamber"]
          },
          "mounts": {
            "type": "array",
            "items": { "$ref": "#/components/schemas/Mount" }
          }
        }
      },
      "Mount": {
        "type": "object",
        "required": ["source", "target"],
        "additionalProperties": false,
        "properties": {
          "type": {
            "type": "string",
            "example": "bind"
          },
          "source": {
            "type": "string",
            "example": "/home/user/chamber"
          },
          "target": {
            "type": "string",
            "example": "/workspace"
          },
          "options": {
            "type": "array",
            "items": { "type": "string" },
            "example": ["rbind", "ro"]
          }
        }
      },
      "RunContainerResponse": {
        "type": "object",
        "required": ["operation_id", "id", "image_digest", "state"],
        "additionalProperties": false,
        "properties": {
          "operation_id": { "type": "string" },
          "id": { "type": "string" },
          "image_digest": { "type": "string" },
          "state": { "$ref": "#/components/schemas/ContainerState" }
        }
      },
      "ListContainersResponse": {
        "type": "object",
        "required": ["containers"],
        "additionalProperties": false,
        "properties": {
          "containers": {
            "type": "array",
            "items": { "$ref": "#/components/schemas/Container" }
          }
        }
      },
      "Container": {
        "type": "object",
        "required": ["id", "operation_id", "image", "image_digest", "runtime", "state", "created_at", "updated_at"],
        "additionalProperties": false,
        "properties": {
          "id": { "type": "string" },
          "operation_id": {
            "type": "string",
            "description": "Execution operation that owns this container outcome. Control-operation IDs are returned in X-Chamber-Operation-ID."
          },
          "image": { "type": "string" },
          "image_digest": { "type": "string" },
          "runtime": { "type": "string" },
          "state": { "$ref": "#/components/schemas/ContainerState" },
          "created_at": { "type": "string", "format": "date-time" },
          "updated_at": { "type": "string", "format": "date-time" },
          "exit_code": { "type": "integer" },
          "error_code": { "type": "string" }
        }
      },
      "ContainerState": {
        "type": "string",
        "enum": ["creating", "created", "starting", "running", "exited", "failed"]
      },
      "ListOperationsResponse": {
        "type": "object",
        "required": ["operations", "pending_cleanups"],
        "additionalProperties": false,
        "properties": {
          "operations": {
            "type": "array",
            "items": { "$ref": "#/components/schemas/Operation" }
          },
          "pending_cleanups": { "type": "integer", "minimum": 0 }
        }
      },
      "Operation": {
        "type": "object",
        "required": ["id", "kind", "state", "resource_id", "started_at", "updated_at"],
        "additionalProperties": false,
        "properties": {
          "id": { "type": "string" },
          "kind": {
            "type": "string",
            "enum": ["pull", "create", "start", "run", "stop", "cancel", "remove", "cleanup"]
          },
          "state": {
            "type": "string",
            "enum": ["running", "succeeded", "failed", "aborted"]
          },
          "resource_id": { "type": "string" },
          "trace_id": { "type": "string" },
          "span_id": { "type": "string" },
          "started_at": { "type": "string", "format": "date-time" },
          "updated_at": { "type": "string", "format": "date-time" },
          "finished_at": { "type": "string", "format": "date-time" },
          "error_code": { "type": "string" }
        }
      },
      "SystemInfoResponse": {
        "type": "object",
        "required": ["startup_probe"],
        "additionalProperties": false,
        "properties": {
          "startup_probe": { "$ref": "#/components/schemas/StartupProbe" }
        }
      },
      "StartupProbe": {
        "type": "object",
        "required": ["passed", "scopes"],
        "additionalProperties": false,
        "properties": {
          "passed": { "type": "boolean" },
          "scopes": {
            "type": "array",
            "items": { "$ref": "#/components/schemas/StartupScope" }
          }
        }
      },
      "StartupScope": {
        "type": "object",
        "required": ["name", "passed"],
        "additionalProperties": false,
        "properties": {
          "name": { "type": "string" },
          "implementation": { "type": "string" },
          "path": { "type": "string" },
          "passed": { "type": "boolean" },
          "error": { "type": "string" }
        }
      },
      "ErrorResponse": {
        "type": "object",
        "required": ["code", "message"],
        "additionalProperties": false,
        "properties": {
          "code": { "type": "string" },
          "message": { "type": "string" }
        }
      }
    }
  }
}
`
