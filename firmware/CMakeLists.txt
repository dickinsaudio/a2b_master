cmake_minimum_required(VERSION 3.13)

# a2b-master v2: USB-CDC -> I2C master + command-list player for an AD242x/AD243x A2B main node.
# Configure with e.g.:
#   cmake -S . -B /tmp/a2b_master_build -DPICO_SDK_PATH=/path/to/pico-sdk -DPICO_BOARD=pico
#   (or -DPICO_BOARD=pico2)
if(NOT DEFINED PICO_BOARD)
    set(PICO_BOARD pico CACHE STRING "Board type")
endif()

include(pico_sdk_import.cmake)

project(a2b_master C CXX ASM)
set(CMAKE_C_STANDARD 11)

pico_sdk_init()

set(A2B_MASTER_SOURCES main.c proto.c cmdlist.c i2c_master.c flash_store.c)
add_executable(a2b_master ${A2B_MASTER_SOURCES})

# Strict warnings for our own sources only (not the SDK's).
set_source_files_properties(${A2B_MASTER_SOURCES} PROPERTIES
    COMPILE_OPTIONS "-Wall;-Wextra;-Wshadow;-Wstrict-prototypes")

# Pin / instance overrides, e.g. -DBRIDGE_SDA_PIN=0 -DBRIDGE_SCL_PIN=1
foreach(opt BRIDGE_I2C_INDEX BRIDGE_SDA_PIN BRIDGE_SCL_PIN BRIDGE_DEFAULT_KHZ AUTORUN_DELAY_MS)
    if(DEFINED ${opt})
        target_compile_definitions(a2b_master PRIVATE ${opt}=${${opt}})
    endif()
endforeach()

target_link_libraries(a2b_master pico_stdlib hardware_i2c hardware_flash pico_flash)

pico_enable_stdio_usb(a2b_master 1)
pico_enable_stdio_uart(a2b_master 0)

pico_add_extra_outputs(a2b_master)
