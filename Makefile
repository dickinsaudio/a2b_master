# a2b_master top level build                                        make help
#
# Needs: cmake, gcc-arm-none-eabi, gcc/g++, python3, and Go (sudo apt install golang-go)
# The Pico SDK and picotool are submodules in extern/ (make submodules).

BUILD    ?= build
BOARD    ?= pico
BTYPE    ?= Release
JOBS     ?= $(shell nproc)
DESKTOP  ?=
PROJECT  ?=

ROOT     := $(CURDIR)
B        := $(ROOT)/$(BUILD)
SDK      := $(ROOT)/extern/pico-sdk
PT_SRC   := $(ROOT)/extern/picotool
PT_INST  := $(B)/picotool-install
PT_DIR   := $(PT_INST)/picotool
FW       := $(B)/firmware-$(BOARD)
GO       ?= go
VERSION  := $(shell git describe --always --dirty 2>/dev/null || echo dev)
GOLDFLAGS = -s -w -X main.version=$(VERSION)

.PHONY: all help submodules picotool firmware firmware2 app app-linux fake native test sim desktop clean

all: firmware app fake            ## firmware (BOARD=pico), Windows+Linux app, fake Pico

help:                             ## this list
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | sed 's/:.*## /	/' | expand -t 18
	@echo "variables: BOARD=pico|pico2  BTYPE=Release|Debug  DESKTOP=<dir for exe/uf2>  PROJECT=<sub-node project for native>"

submodules:                       ## fetch the Pico SDK (+tinyusb) and picotool
	git submodule update --init extern/pico-sdk extern/picotool
	git -C extern/pico-sdk submodule update --init lib/tinyusb

$(SDK)/CMakeLists.txt $(PT_SRC)/CMakeLists.txt:
	$(MAKE) submodules

picotool: $(PT_DIR)/picotoolConfig.cmake  ## host picotool, built once into build/

$(PT_DIR)/picotoolConfig.cmake: | $(SDK)/CMakeLists.txt $(PT_SRC)/CMakeLists.txt
	cmake -S $(PT_SRC) -B $(B)/picotool -DPICO_SDK_PATH=$(SDK) -DPICOTOOL_FLAT_INSTALL=1 \
	      -DCMAKE_INSTALL_PREFIX=$(PT_INST) -DCMAKE_BUILD_TYPE=Release $(if $(shell pkg-config --exists libusb-1.0 2>/dev/null && echo y),,-DPICOTOOL_NO_LIBUSB=1)
	cmake --build $(B)/picotool -j$(JOBS)
	cmake --install $(B)/picotool

firmware: picotool                ## Pico firmware -> build/firmware-$(BOARD)/a2b_master.uf2
	cmake -S firmware -B $(FW) -DPICO_SDK_PATH=$(SDK) -Dpicotool_DIR=$(PT_DIR) -DPICO_BOARD=$(BOARD) \
	      -DCMAKE_BUILD_TYPE=$(BTYPE) -DCMAKE_EXPORT_COMPILE_COMMANDS=ON
	cmake --build $(FW) -j$(JOBS)
	ln -sf $(FW)/compile_commands.json $(B)/compile_commands.json
	@echo "==> $(FW)/a2b_master.uf2"

firmware2:                        ## same for an RP2350 Pico 2
	$(MAKE) firmware BOARD=pico2

app:                              ## a2b_master.exe (Windows) and the Linux build
	@command -v $(GO) >/dev/null || { echo "Go not found: sudo apt install golang-go"; exit 1; }
	cd app && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(GOLDFLAGS)" -o $(B)/app/a2b_master.exe .
	cd app && CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(GOLDFLAGS)" -o $(B)/app/a2b_master .
	@echo "==> $(B)/app/a2b_master.exe"

fake:                             ## native fake Pico (real protocol + player, simulated AD2437)
	firmware/test/build_fake.sh $(B)/fake

native:                           ## device tunnel harness (needs PROJECT=<sub-node project> unless a2b_master is its submodule)
	$(if $(PROJECT),PROJECT=$(PROJECT)) device/test/build_native.sh $(B)/native

test: app fake                    ## Go unit tests + fake Pico test suite
	cd app && $(GO) test ./...
	python3 firmware/test/test_fake.py --fake $(B)/fake/fake_pico

sim: app fake                     ## run app + fake Pico without hardware, GUI on http://127.0.0.1:8000
	@echo "fake Pico on tcp:127.0.0.1:9600 - connect to it in the GUI (Ctrl-C to stop)"
	$(B)/fake/fake_pico --port 9600 & trap "kill $$!" EXIT; $(B)/app/a2b_master

desktop: app firmware             ## copy exe + uf2 to DESKTOP=<dir>
	@test -n "$(DESKTOP)" || { echo "set DESKTOP=<dir>"; exit 1; }
	mkdir -p "$(DESKTOP)"
	cp $(B)/app/a2b_master.exe $(FW)/a2b_master.uf2 "$(DESKTOP)/"

clean:                            ## remove build/ (keeps the built picotool unless CLEAN_ALL=1)
	find $(B) -mindepth 1 -maxdepth 1 $(if $(CLEAN_ALL),,! -name 'picotool*') -exec rm -rf {} +
