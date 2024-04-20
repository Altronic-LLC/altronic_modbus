package modbus

import (
	"fmt"
)

const (
	blCmdWake        uint8 = 255
	blCmdRdFilespec  uint8 = 9
	blCmdWrFilespec  uint8 = 1
	blCmdEraseApp    uint8 = 2
	blCmdRdApp       uint8 = 3
	blCmdWrApp       uint8 = 4
	blCmdValidateApp uint8 = 5
	blCmdReset       uint8 = 6
	blCmdBlInfo      uint8 = 7
	blCmdBlankCheck  uint8 = 8
	blCmdInitWrApp   uint8 = 10
	blCmdFinWrApp    uint8 = 11

	blErrWake   uint8 = 255
	blErrCmd    uint8 = 254
	blErrCrc    uint8 = 253
	blErrOffset uint8 = 252
	blErrData   uint8 = 251
)

func (mc *ModbusClient) BootloaderWake() (err error) {
	var req *pdu
	var res *pdu

	mc.lock.Lock()
	defer mc.lock.Unlock()

	// create and fill in the request object
	req = &pdu{
		unitId:       mc.unitId,
		functionCode: fcBootloader,
	}

	req.payload = append(req.payload, blCmdWake)

	b := uint16ToBytes(BIG_ENDIAN, 0x0000) // offset
	req.payload = append(req.payload, b...)

	b = uint16ToBytes(BIG_ENDIAN, 0x0000) // length
	req.payload = append(req.payload, b...)

	// run the request across the transport and wait for a response
	res, err = mc.executeRequest(req)
	if err != nil {
		return
	}

	// validate the response code
	switch {
	case res.functionCode == req.functionCode:
		return

	case res.functionCode == (req.functionCode | 0x80):
		if len(res.payload) != 1 {
			err = ErrProtocolError
			return
		}

		err = mapExceptionCodeToError(res.payload[0])

	default:
		err = ErrProtocolError
		mc.logger.Warningf("unexpected response code (%v)", res.functionCode)
	}

	return

}

func (mc *ModbusClient) BootloaderEraseApp() (err error) {
	var req *pdu
	var res *pdu

	mc.lock.Lock()
	defer mc.lock.Unlock()

	// create and fill in the request object
	req = &pdu{
		unitId:       mc.unitId,
		functionCode: fcBootloader,
	}

	req.payload = append(req.payload, blCmdEraseApp)
	b := uint16ToBytes(BIG_ENDIAN, 0x0000) // offset
	req.payload = append(req.payload, b...)

	b = uint16ToBytes(BIG_ENDIAN, 0x0000) // length
	req.payload = append(req.payload, b...)

	// run the request across the transport and wait for a response
	res, err = mc.executeRequest(req)
	if err != nil {
		return
	}

	// validate the response code
	switch {
	case res.functionCode == req.functionCode:
		return

	case res.functionCode == (req.functionCode | 0x80):
		if len(res.payload) != 1 {
			err = ErrProtocolError
			return
		}

		err = mapExceptionCodeToError(res.payload[0])

	default:
		err = ErrProtocolError
		mc.logger.Warningf("unexpected response code (%v)", res.functionCode)
	}

	return

}

func (mc *ModbusClient) BootloaderBlankCheck() (err error) {
	var req *pdu
	var res *pdu

	mc.lock.Lock()
	defer mc.lock.Unlock()

	// create and fill in the request object
	req = &pdu{
		unitId:       mc.unitId,
		functionCode: fcBootloader,
	}
	req.payload = append(req.payload, blCmdBlankCheck)

	b := uint16ToBytes(BIG_ENDIAN, 0x0000) // offset
	req.payload = append(req.payload, b...)

	b = uint16ToBytes(BIG_ENDIAN, 0x0000) // length
	req.payload = append(req.payload, b...)

	// run the request across the transport and wait for a response
	res, err = mc.executeRequest(req)
	if err != nil {
		return
	}

	// validate the response code
	switch {
	case res.functionCode == req.functionCode:
		return

	case res.functionCode == (req.functionCode | 0x80):
		if len(res.payload) != 1 {
			err = ErrProtocolError
			return
		}

		err = mapExceptionCodeToError(res.payload[0])

	default:
		err = ErrProtocolError
		mc.logger.Warningf("unexpected response code (%v)", res.functionCode)
	}

	return

}

func (mc *ModbusClient) BootloaderIdent() (err error) {
	var req *pdu
	var res *pdu

	mc.lock.Lock()
	defer mc.lock.Unlock()

	// create and fill in the request object
	req = &pdu{
		unitId:       mc.unitId,
		functionCode: fcReadSlaveId,
	}

	// run the request across the transport and wait for a response
	res, err = mc.executeRequest(req)
	if err != nil {
		return
	}

	// validate the response code
	switch {
	case res.functionCode == req.functionCode:
		fmt.Printf("Ident -  '%s' \n",
			res.payload)
		return

	case res.functionCode == (req.functionCode | 0x80):
		if len(res.payload) != 1 {
			err = ErrProtocolError
			return
		}

		err = mapExceptionCodeToError(res.payload[0])

	default:
		err = ErrProtocolError
		mc.logger.Warningf("unexpected response code (%v)", res.functionCode)
	}

	return

}

func (mc *ModbusClient) BootloaderWriteApp(blData []byte, blOffset uint32, blLength uint8) (err error) {
	var req *pdu
	var res *pdu

	mc.lock.Lock()
	defer mc.lock.Unlock()

	// create and fill in the request object
	req = &pdu{
		unitId:       mc.unitId,
		functionCode: fcBootloader,
	}
	req.payload = append(req.payload, blCmdWrApp)

	b := uint32ToBytes(BIG_ENDIAN, HIGH_WORD_FIRST, blOffset) // offset
	req.payload = append(req.payload, b[1], b[2], b[3])

	req.payload = append(req.payload, blLength)

	req.payload = append(req.payload, blData...)

	// run the request across the transport and wait for a response
	res, err = mc.executeRequest(req)
	if err != nil {
		return
	}

	// validate the response code
	switch {
	case res.functionCode == req.functionCode:
		return

	case res.functionCode == (req.functionCode | 0x80):
		if len(res.payload) != 1 {
			err = ErrProtocolError
			return
		}

		err = mapExceptionCodeToError(res.payload[0])

	default:
		err = ErrProtocolError
		mc.logger.Warningf("unexpected response code (%v)", res.functionCode)
	}

	return

}

func (mc *ModbusClient) BootloaderReadApp(blOffset uint32, blLength uint8) (blData []byte, err error) {
	var req *pdu
	var res *pdu

	mc.lock.Lock()
	defer mc.lock.Unlock()

	// create and fill in the request object
	req = &pdu{
		unitId:       mc.unitId,
		functionCode: fcBootloader,
	}
	req.payload = append(req.payload, blCmdRdApp)

	b := uint32ToBytes(BIG_ENDIAN, HIGH_WORD_FIRST, blOffset) // offset
	req.payload = append(req.payload, b[1], b[2], b[3])

	req.payload = append(req.payload, blLength)

	// run the request across the transport and wait for a response
	res, err = mc.executeRequest(req)
	if err != nil {
		return
	}

	// validate the response code
	switch {
	case res.functionCode == req.functionCode:

		blData = res.payload[1:]
		return

	case res.functionCode == (req.functionCode | 0x80):
		if len(res.payload) != 1 {
			err = ErrProtocolError
			return
		}

		err = mapExceptionCodeToError(res.payload[0])

	default:
		err = ErrProtocolError
		mc.logger.Warningf("unexpected response code (%v)", res.functionCode)
	}

	return

}

func (mc *ModbusClient) BootloaderWriteFileSpec(blfwsz uint32, blfwcrc uint32) (err error) {
	var req *pdu
	var res *pdu

	var blOffset uint32 = 0x0000
	var blLength uint8 = 0x8

	mc.lock.Lock()
	defer mc.lock.Unlock()

	// create and fill in the request object
	req = &pdu{
		unitId:       mc.unitId,
		functionCode: fcBootloader,
	}
	req.payload = append(req.payload, blCmdWrApp)

	b := uint32ToBytes(BIG_ENDIAN, HIGH_WORD_FIRST, blOffset) // offset
	req.payload = append(req.payload, b[1], b[2], b[3])

	req.payload = append(req.payload, blLength)

	b = uint32ToBytes(BIG_ENDIAN, HIGH_WORD_FIRST, blfwsz) // offset

	req.payload = append(req.payload, b[1], b[2], b[3], b[4])

	b = uint32ToBytes(BIG_ENDIAN, HIGH_WORD_FIRST, blfwcrc) // offset

	req.payload = append(req.payload, b[1], b[2], b[3], b[4])

	// run the request across the transport and wait for a response
	res, err = mc.executeRequest(req)
	if err != nil {
		return
	}

	// validate the response code
	switch {
	case res.functionCode == req.functionCode:
		return

	case res.functionCode == (req.functionCode | 0x80):
		if len(res.payload) != 1 {
			err = ErrProtocolError
			return
		}

		err = mapExceptionCodeToError(res.payload[0])

	default:
		err = ErrProtocolError
		mc.logger.Warningf("unexpected response code (%v)", res.functionCode)
	}

	return

}
