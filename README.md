# Sonic Peer

This repository contains `sonicpeer`, a lightweight P2P gossip agent for the Sonic network.

## Overview

The `sonicpeer` acts as a sentry node, gossiping transactions and events without maintaining local state or participating in consensus. It is designed to proxy for a trusted backend full node, enhancing network security and connectivity.

## Getting Started

### Prerequisites

- Go (version 1.20 or later)
- A running Sonic full node (backend)

### Building

To build the `sonicpeer` executable:

```sh
go build -o build/sonicpeer ./cmd/sonicpeer
```

### Running

The recommended way to run the node is using the provided `Makefile`, which simplifies the process.

1.  **Configure the Backend Node**

    First, create a local environment file by copying the example:
    ```sh
    cp .env.example .env
    ```
    Next, edit the new `.env` file to set the `BACKEND_URL` to the WebSocket URL of your running Sonic full node.

2.  **Run the Node**

    With the configuration in place, you can start the sentry node with a single command:
    ```sh
    make run
    ```

#### Manual Execution

Alternatively, you can run the executable directly and provide the URL as a command-line flag:

```sh
./build/sonicpeer --url <YOUR_BACKEND_NODE_URL>
```

For a full list of available flags, run:

```sh
./build/sonicpeer --help
```

## Contributing

Contributions are welcome! Please feel free to submit a pull request.