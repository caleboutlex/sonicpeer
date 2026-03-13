# Default backend URL. Can be overridden by an environment variable.
# e.g., BACKEND_URL="ws://your-node-ip:18546" make run
BACKEND_URL ?= ws://127.0.0.1:18546

# Load environment variables from .env file if it exists.
# This allows you to set your backend URL in a .env file.
ifneq (,$(wildcard ./.env))
	include .env
	export
endif

# Phony targets are not real files. Declaring them avoids conflicts with files of the same name.
.PHONY: sonicpeer run test clean vet lint

# sonicpeer: Builds the sonicpeer executable from source and makes it runnable.
sonicpeer:
	@echo "Building sonicpeer executable..."
	go build \
	    -o build/sonicpeer \
	    ./cmd/sonicpeer
	chmod +x ./build/sonicpeer

# run: Runs the compiled sonicpeer node.
# It depends on the 'sonicpeer' target, so it will build the binary first if it's not up-to-date.
run: sonicpeer
	@echo "Starting sonicpeer..."
	./build/sonicpeer \
	--datadir ./build/data \
	--maxpeers 1000 \
	--url "$(BACKEND_URL)" \
	--port 5051

# vet: Runs go vet to check for suspicious constructs in the code.
vet:
	@echo "Vetting code..."
	go vet ./...

# lint: Runs golangci-lint to find style issues and potential bugs.
# Requires golangci-lint to be installed: https://golangci-lint.run/usage/install/
lint:
	@echo "Linting code..."
	golangci-lint run ./...

# clean: Removes the build directory to clean up all compiled artifacts and data.
clean:
	@echo "Cleaning up build artifacts..."
	rm -rf build

# test: Runs the application directly from source using 'go run'.
# This is useful for quick, iterative testing without needing to build the binary first.
test: 
	@echo "Running sonicpeer in test mode..."
	go run ./cmd/sonicpeer \
	--datadir ./build/data \
	--maxpeers 1000 \
	--url "$(BACKEND_URL)" \
	--port 5051