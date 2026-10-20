// Portable I2C transfer status codes shared by the line protocol, the command-list player
// and the platform I2C back ends (RP2040 hardware or the native simulator).
#ifndef I2C_STATUS_H
#define I2C_STATUS_H

enum i2c_status {
    I2C_ST_OK = 0,   // transfer complete, everything ACKed
    I2C_ST_NAK,      // address or a data byte not acknowledged
    I2C_ST_ARB,      // arbitration lost (another master on the bus)
    I2C_ST_TIMEOUT,  // bus stuck / busy (back end has attempted bus recovery)
    I2C_ST_ABRT,     // other controller abort; detail = abort source
};

#endif
