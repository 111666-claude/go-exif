package exif

import (
	"errors"
	"math/big"

	"encoding/binary"

	"github.com/dsoprea/go-logging"

	"github.com/dsoprea/go-exif/v3/common"
)

var (
	// ErrNoGpsIfd means that the EXIF data does not contain a GPS IFD. It is
	// returned as a signal (not a failure) so that callers can distinguish
	// "nothing to sanitize" from a genuine processing error.
	ErrNoGpsIfd = errors.New("no GPS IFD found")

	// ErrGpsCoordinateMissing means that one of the coordinate components
	// (latitude/longitude, or their references) is absent or malformed.
	ErrGpsCoordinateMissing = errors.New("GPS coordinate component missing or invalid")

	// ErrGpsPrecisionInvalid means that the requested coordinate precision is
	// outside of the supported range.
	ErrGpsPrecisionInvalid = errors.New("GPS precision invalid")

	// ErrIfdStructureInvalid means that the IFD graph could not be safely
	// re-encoded, e.g. because IFDs reference each other in a cycle or an
	// offset does not point at readable data.
	ErrIfdStructureInvalid = errors.New("IFD structure invalid")
)

const (
	// MaxGpsDecimalPlaces is the largest number of decimal places that
	// SanitizeGps will accept for coordinate truncation. It is bounded by the
	// 32-bit numerator/denominator fields used for EXIF RATIONAL values.
	MaxGpsDecimalPlaces = 7

	// gpsIfdOffsetSanityLimit is the minimum size an EXIF blob must have for a
	// first-IFD offset to be considered plausible.
	gpsIfdOffsetSanityLimit = uint32(ExifSignatureLength)
)

// GpsSanitizeStrategy controls how GPS metadata is sanitized.
//
// If DropGpsIfd is true, the GPS IFD (and the GPSTag pointer in IFD0) is
// removed entirely and all other fields are ignored.
//
// Otherwise, the GPS IFD is kept but GPSLatitude (0x0002) and GPSLongitude
// (0x0004) are truncated (towards zero, at the given number of decimal places
// of the decimal-degree value) and re-encoded. All other GPS tags (version,
// altitude, timestamp, datum, ...) are preserved verbatim. DecimalPlaces must
// be in [0, MaxGpsDecimalPlaces].
type GpsSanitizeStrategy struct {
	// DropGpsIfd removes the whole GPS IFD when true.
	DropGpsIfd bool

	// DecimalPlaces is the number of decimal places of the decimal-degree
	// coordinate to retain. Only used when DropGpsIfd is false.
	DecimalPlaces int
}

// SanitizeGps parses the given raw EXIF block, applies the GPS sanitization
// strategy, and returns a new, self-contained EXIF block that can be read back
// with the package's regular parser.
//
// The input slice is never modified. The original byte-order, every
// non-selected IFD/tag (including unknown and private tags), and thumbnail
// references are preserved.
//
// If the data has no GPS IFD, ErrNoGpsIfd is returned together with the
// original byte-slice (the caller may use it as-is). Structural problems
// (unreadable offsets, IFD reference cycles, missing coordinate components,
// invalid precision) produce wrapped, sentinel-typed errors carrying the
// offending IFD/tag context.
func SanitizeGps(rawExif []byte, strategy GpsSanitizeStrategy) (sanitizedExif []byte, err error) {
	defer func() {
		if state := recover(); state != nil {
			err = log.Wrap(state.(error))
		}
	}()

	if len(rawExif) == 0 {
		return nil, log.Wrap(ErrIfdStructureInvalid)
	}

	eh, err := ParseExifHeader(rawExif)
	if err != nil {
		// ParseExifHeader returns ErrNoExif for any malformed header.
		return nil, err
	}

	if eh.FirstIfdOffset < gpsIfdOffsetSanityLimit || eh.FirstIfdOffset >= uint32(len(rawExif)) {
		return nil, log.Errorf("%w: first IFD offset (0x%08x) is outside of the %d-byte EXIF block",
			ErrIfdStructureInvalid, eh.FirstIfdOffset, len(rawExif))
	}

	if strategy.DropGpsIfd == false {
		if strategy.DecimalPlaces < 0 || strategy.DecimalPlaces > MaxGpsDecimalPlaces {
			return nil, log.Errorf("%w: decimal places (%d) must be between 0 and %d",
				ErrGpsPrecisionInvalid, strategy.DecimalPlaces, MaxGpsDecimalPlaces)
		}
	}

	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()

	_, index, err := Collect(im, ti, rawExif)
	if err != nil {
		// Bad offsets and truncated IFD tables surface here (typically wrapped
		// io.EOF/ErrOffsetInvalid).
		return nil, log.Errorf("%w: unable to collect IFDs: %s", ErrIfdStructureInvalid, err)
	}

	if err := validateIfdGraph(index); err != nil {
		return nil, err
	}

	gpsIfd, err := index.RootIfd.ChildWithIfdPath(exifcommon.IfdGpsInfoStandardIfdIdentity)
	if err != nil {
		if log.Is(err, ErrTagNotFound) == true {
			// Nothing to do. Hand the original bytes back unchanged so the
			// caller never has to special-case GPS-less media.
			return rawExif, ErrNoGpsIfd
		}

		log.Panic(err)
	}

	rootIb := NewIfdBuilderFromExistingChain(index.RootIfd)

	gpsTagId := exifcommon.IfdGpsInfoStandardIfdIdentity.TagId()

	if strategy.DropGpsIfd == true {
		// Deleting the GPSTag (0x8825) entry also removes the child GPS
		// IfdBuilder attached to it.
		err = rootIb.DeleteFirst(gpsTagId)
		log.PanicIf(err)
	} else {
		gpsIb, err := rootIb.ChildWithTagId(gpsTagId)
		log.PanicIf(err)

		err = truncateGpsCoordinates(gpsIfd, gpsIb, eh.ByteOrder, strategy.DecimalPlaces)
		log.PanicIf(err)
	}

	ibe := NewIfdByteEncoder()

	sanitizedExif, err = ibe.EncodeToExif(rootIb)
	log.PanicIf(err)

	return sanitizedExif, nil
}

// validateIfdGraph rejects IFD graphs that cannot be represented by the
// encoder: IFDs sharing an on-disk offset (which is what mutually-referencing
// / cyclic IFDs collapse to after collection) would be encoded ambiguously.
func validateIfdGraph(index IfdIndex) (err error) {
	defer func() {
		if state := recover(); state != nil {
			err = log.Wrap(state.(error))
		}
	}()

	seenOffsets := make(map[uint32]string)
	for _, ifd := range index.Ifds {
		offset := ifd.Offset()
		ifdPath := ifd.IfdIdentity().String()

		if previousPath, found := seenOffsets[offset]; found == true {
			return log.Errorf(
				"%w: IFD [%s] at offset (0x%08x) is also referenced as [%s]; mutually-referencing IFDs are not supported",
				ErrIfdStructureInvalid, ifdPath, offset, previousPath)
		}

		seenOffsets[offset] = ifdPath
	}

	return nil
}

// truncateGpsCoordinates reads the four coordinate tags from the parsed GPS
// IFD, truncates each coordinate to the requested decimal precision, and
// replaces the corresponding entries in the builder.
//
// If neither coordinate is present at all, the GPS IFD is returned untouched:
// it carries no precise location, so there is nothing to sanitize. If exactly
// one coordinate (or any single component) is present, that is malformed input
// and ErrGpsCoordinateMissing is returned with the offending tag context.
func truncateGpsCoordinates(gpsIfd *Ifd, gpsIb *IfdBuilder, byteOrder binary.ByteOrder, decimalPlaces int) (err error) {
	defer func() {
		if state := recover(); state != nil {
			err = log.Wrap(state.(error))
		}
	}()

	_, latRefFound := gpsIfd.EntriesByTagId()[TagLatitudeRefId]
	_, latFound := gpsIfd.EntriesByTagId()[TagLatitudeId]
	_, lonRefFound := gpsIfd.EntriesByTagId()[TagLongitudeRefId]
	_, lonFound := gpsIfd.EntriesByTagId()[TagLongitudeId]

	anyLatitude := latRefFound || latFound
	anyLongitude := lonRefFound || lonFound

	if anyLatitude == false && anyLongitude == false {
		// No coordinates at all (e.g. an IFD holding only GPSVersionID).
		return nil
	}

	latitude, _, err := readGpsCoordinate(gpsIfd, TagLatitudeRefId, TagLatitudeId, "latitude")
	log.PanicIf(err)

	longitude, _, err := readGpsCoordinate(gpsIfd, TagLongitudeRefId, TagLongitudeId, "longitude")
	log.PanicIf(err)

	// decimalToDmsRationals performs the truncation itself (fixed-point
	// integer arithmetic), so no separate rounding step is needed. The
	// N/S/E/W reference tags encode the sign and are left untouched.
	latitudeRationals, err := decimalToDmsRationals(latitude, decimalPlaces)
	log.PanicIf(err)

	longitudeRationals, err := decimalToDmsRationals(longitude, decimalPlaces)
	log.PanicIf(err)

	err = replaceGpsRationalTag(gpsIb, byteOrder, TagLatitudeId, latitudeRationals)
	log.PanicIf(err)

	err = replaceGpsRationalTag(gpsIb, byteOrder, TagLongitudeId, longitudeRationals)
	log.PanicIf(err)

	return nil
}

// readGpsCoordinate resolves one coordinate (reference plus three rationals)
// to a signed decimal degree value.
func readGpsCoordinate(gpsIfd *Ifd, refTagId, coordinateTagId uint16, label string) (decimal float64, ref byte, err error) {
	defer func() {
		if state := recover(); state != nil {
			err = log.Wrap(state.(error))
		}
	}()

	refEntries, found := gpsIfd.EntriesByTagId()[refTagId]
	if found == false || len(refEntries) == 0 {
		return 0, 0, log.Errorf("%w: GPS %s reference tag (0x%04x) is missing",
			ErrGpsCoordinateMissing, label, refTagId)
	}

	refValue, err := refEntries[0].Value()
	log.PanicIf(err)

	refPhrase, ok := refValue.(string)
	if ok == false || len(refPhrase) == 0 {
		return 0, 0, log.Errorf("%w: GPS %s reference tag (0x%04x) does not hold an ASCII value",
			ErrGpsCoordinateMissing, label, refTagId)
	}

	ref = refPhrase[0]

	coordinateEntries, found := gpsIfd.EntriesByTagId()[coordinateTagId]
	if found == false || len(coordinateEntries) == 0 {
		return 0, 0, log.Errorf("%w: GPS %s tag (0x%04x) is missing",
			ErrGpsCoordinateMissing, label, coordinateTagId)
	}

	ite := coordinateEntries[0]
	if ite.TagType() != exifcommon.TypeRational {
		return 0, 0, log.Errorf("%w: GPS %s tag (0x%04x) has type [%s], RATIONAL is required",
			ErrGpsCoordinateMissing, label, coordinateTagId, ite.TagType())
	}

	value, err := ite.Value()
	log.PanicIf(err)

	rationals, ok := value.([]exifcommon.Rational)
	if ok == false || len(rationals) != 3 {
		return 0, 0, log.Errorf("%w: GPS %s tag (0x%04x) must contain exactly three rationals, has (%d)",
			ErrGpsCoordinateMissing, label, coordinateTagId, len(rationals))
	}

	for i, rational := range rationals {
		if rational.Denominator == 0 {
			return 0, 0, log.Errorf("%w: GPS %s tag (0x%04x) rational (%d) has a zero denominator",
				ErrGpsCoordinateMissing, label, coordinateTagId, i)
		}
	}

	gd, err := NewGpsDegreesFromRationals(string(ref), rationals)
	log.PanicIf(err)

	return gd.Decimal(), ref, nil
}

// replaceGpsRationalTag swaps one RATIONAL GPS tag in the builder for a newly
// encoded value of the same type.
func replaceGpsRationalTag(gpsIb *IfdBuilder, byteOrder binary.ByteOrder, tagId uint16, rationals []exifcommon.Rational) (err error) {
	defer func() {
		if state := recover(); state != nil {
			err = log.Wrap(state.(error))
		}
	}()

	it, err := gpsIb.tagIndex.Get(gpsIb.IfdIdentity(), tagId)
	log.PanicIf(err)

	bt := NewStandardBuilderTag(gpsIb.IfdIdentity().UnindexedString(), it, byteOrder, rationals)

	err = gpsIb.Replace(tagId, bt)
	log.PanicIf(err)

	return nil
}

// decimalToDmsRationals converts a signed decimal-degree coordinate to the
// three unsigned EXIF rationals (degrees, minutes, seconds), truncating (not
// rounding) the coordinate to decimalPlaces decimal places. All arithmetic
// after the initial fixed-point conversion uses integers (math/big) so that
// floating-point rounding can not leak extra precision past the requested
// number of decimal places. The reduced seconds rational is guaranteed to fit
// in 32-bit fields for precisions up to MaxGpsDecimalPlaces decimal places.
func decimalToDmsRationals(decimal float64, decimalPlaces int) (rationals []exifcommon.Rational, err error) {
	defer func() {
		if state := recover(); state != nil {
			err = log.Wrap(state.(error))
		}
	}()

	if decimal < 0 {
		decimal = -decimal
	}

	// Convert the float coordinate to a fixed-point integer in units of
	// 10^-decimalPlaces degrees. int64() truncates towards zero, which is
	// exactly the required behavior. At the maximum supported precision (7)
	// even 180 degrees only reaches 1.8e9, well inside int64 and the exact
	// integer range of float64 (2^53).
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimalPlaces)), nil)

	scaled := new(big.Int).SetInt64(int64(decimal * float64(scale.Int64())))

	sixteen := big.NewInt(60)

	degrees := new(big.Int).Quo(scaled, scale)

	remainder := new(big.Int).Rem(scaled, scale)
	minutesScaled := new(big.Int).Mul(remainder, sixteen)
	minutes := new(big.Int).Quo(minutesScaled, scale)

	minuteRemainder := new(big.Int).Rem(minutesScaled, scale)
	secondsScaled := new(big.Int).Mul(minuteRemainder, sixteen)

	// secondsScaled is the seconds value expressed in the fixed-point unit.
	// The EXIF seconds rational is therefore (secondsScaled / scale).
	num := new(big.Int).Set(secondsScaled)
	den := new(big.Int).Set(scale)

	gcd := new(big.Int)
	gcd.GCD(nil, nil, num, den)

	num.Div(num, gcd)
	den.Div(den, gcd)

	maxUint32 := new(big.Int).SetUint64(uint64(^uint32(0)))
	if num.Cmp(maxUint32) > 0 || den.Cmp(maxUint32) > 0 {
		return nil, log.Errorf("%w: truncated coordinate does not fit in 32-bit rational fields",
			ErrGpsPrecisionInvalid)
	}

	rationals = []exifcommon.Rational{
		{Numerator: uint32(degrees.Uint64()), Denominator: 1},
		{Numerator: uint32(minutes.Uint64()), Denominator: 1},
		{Numerator: uint32(num.Uint64()), Denominator: uint32(den.Uint64())},
	}

	return rationals, nil
}
