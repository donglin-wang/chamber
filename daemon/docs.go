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
    "description": "HTTP-only Chamber daemon surface for pulling OCI images, running containers, listing containers, and reading stored logs."
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
          "state": { "type": "string" }
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
          "operation_id": { "type": "string" },
          "image": { "type": "string" },
          "image_digest": { "type": "string" },
          "runtime": { "type": "string" },
          "state": { "type": "string" },
          "created_at": { "type": "string", "format": "date-time" },
          "updated_at": { "type": "string", "format": "date-time" },
          "exit_code": { "type": "integer" },
          "error_code": { "type": "string" }
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
