package exif

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"math"
	"path"
	"reflect"
	"sort"
	"testing"

	"github.com/dsoprea/go-logging"

	"github.com/dsoprea/go-exif/v3/common"
	"github.com/dsoprea/go-exif/v3/undefined"
)

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

// collectTags returns a stable, comparable description of every tag in the
// EXIF block (excluding child-IFD pointer tags, which describe structure
// rather than user data).
func collectTags(t *testing.T, rawExif []byte) []string {
	t.Helper()

	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()

	_, index, err := Collect(im, ti, rawExif)
	log.PanicIf(err)

	lines := make([]string, 0)

	var walk func(ifd *Ifd)
	walk = func(ifd *Ifd) {
		for _, ite := range ifd.Entries() {
			if ite.ChildIfdPath() != "" {
				continue
			}

			if ite.IsThumbnailOffset() || ite.IsThumbnailSize() {
				// Compared separately via the thumbnail bytes.
				continue
			}

			rawBytes, err := ite.GetRawBytes()
			if err != nil {
				if err == exifundefined.ErrUnparseableValue ||
					err == exifcommon.ErrUnhandledUndefinedTypedTag {
					rawBytes = []byte(fmt.Sprintf("unparseable:%d", ite.UnitCount()))
				} else {
					log.Panic(err)
				}
			}

			lines = append(lines, fmt.Sprintf("%s|0x%04x|%s|%d|%x",
				ite.IfdPath(), ite.TagId(), ite.TagType(), ite.UnitCount(), rawBytes))
		}

		for _, child := range ifd.Children() {
			walk(child)
		}
	}

	walk(index.RootIfd)

	sort.Strings(lines)
	return lines
}

// isCoordinateTagLine reports whether a collectTags() line describes one of
// the four GPS coordinate tags that sanitization is allowed to change.
func isCoordinateTagLine(line string) bool {
	const prefix = "IFD/GPSInfo|"
	if bytes.HasPrefix([]byte(line), []byte(prefix)) == false {
		return false
	}

	tagId := line[len(prefix) : len(prefix)+6]
	switch tagId {
	case "0x0001", "0x0002", "0x0003", "0x0004":
		return true
	}

	return false
}

// gpsCoordinateFromExif re-parses and returns the signed decimal lat/lon.
func gpsCoordinateFromExif(t *testing.T, rawExif []byte) (lat, lon float64) {
	t.Helper()

	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()

	_, index, err := Collect(im, ti, rawExif)
	log.PanicIf(err)

	gpsIfd, err := index.RootIfd.ChildWithIfdPath(exifcommon.IfdGpsInfoStandardIfdIdentity)
	log.PanicIf(err)

	gi, err := gpsIfd.GpsInfo()
	log.PanicIf(err)

	return gi.Latitude.Decimal(), gi.Longitude.Decimal()
}

func hasGpsIfd(t *testing.T, rawExif []byte) bool {
	t.Helper()

	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()

	_, index, err := Collect(im, ti, rawExif)
	log.PanicIf(err)

	_, err = index.RootIfd.ChildWithIfdPath(exifcommon.IfdGpsInfoStandardIfdIdentity)
	return err == nil
}

func getThumbnailBytes(t *testing.T, rawExif []byte) []byte {
	t.Helper()

	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()

	_, index, err := Collect(im, ti, rawExif)
	log.PanicIf(err)

	for _, ifd := range index.Ifds {
		data, err := ifd.Thumbnail()
		if err == nil {
			return data
		} else if log.Is(err, ErrNoThumbnail) == false {
			log.Panic(err)
		}
	}

	return nil
}

// getGpsSampleExif extracts the raw EXIF block from one of the GPS test
// images.
func getGpsSampleExif(t *testing.T, filename string) []byte {
	t.Helper()

	assetsPath := exifcommon.GetTestAssetsPath()
	filepath := path.Join(assetsPath, filename)

	rawExif, err := SearchFileAndExtractExif(filepath)
	log.PanicIf(err)

	return rawExif
}

// buildSyntheticGpsExif constructs an EXIF block with a private/undefined tag
// in IFD0, an Exif IFD, a GPS IFD, and (optionally) IFD1 with a thumbnail.
func buildSyntheticGpsExif(t *testing.T, byteOrder binary.ByteOrder, includeThumbnail bool) []byte {
	t.Helper()

	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()

	rootIb := NewIfdBuilder(im, ti, exifcommon.IfdStandardIfdIdentity, byteOrder)

	err = rootIb.AddStandard(0x010f, "AcmeCam") // Make
	log.PanicIf(err)

	err = rootIb.AddStandard(0x0110, "Model X") // Model
	log.PanicIf(err)

	exifIb := NewIfdBuilder(im, ti, exifcommon.IfdExifStandardIfdIdentity, byteOrder)
	err = exifIb.AddStandard(0x829a, []exifcommon.Rational{{Numerator: 1, Denominator: 640}})
	log.PanicIf(err)

	// Private camera-maker data: MakerNote (0x927c) is a registered
	// UNDEFINED tag, so we can carry opaque vendor bytes through it.
	makerNote := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22, 0x33}
	makerBt := NewBuilderTag(
		exifcommon.IfdExifStandardIfdIdentity.UnindexedString(),
		0x927c,
		exifcommon.TypeUndefined,
		NewIfdBuilderTagValueFromBytes(makerNote),
		byteOrder)
	err = exifIb.Add(makerBt)
	log.PanicIf(err)

	err = rootIb.AddChildIb(exifIb)
	log.PanicIf(err)

	gpsIb := NewIfdBuilder(im, ti, exifcommon.IfdGpsInfoStandardIfdIdentity, byteOrder)
	err = gpsIb.AddStandard(TagGpsVersionId, []byte{2, 3, 0, 0})
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLatitudeRefId, "N")
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLatitudeId, []exifcommon.Rational{
		{Numerator: 48, Denominator: 1},
		{Numerator: 51, Denominator: 1},
		{Numerator: 24900000, Denominator: 1000000}, // 24.9"
	})
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLongitudeRefId, "E")
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLongitudeId, []exifcommon.Rational{
		{Numerator: 2, Denominator: 1},
		{Numerator: 17, Denominator: 1},
		{Numerator: 55900000, Denominator: 1000000}, // 55.9"
	})
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagAltitudeId, []exifcommon.Rational{{Numerator: 35, Denominator: 1}})
	log.PanicIf(err)
	err = rootIb.AddChildIb(gpsIb)
	log.PanicIf(err)

	if includeThumbnail {
		ifd1 := NewIfdBuilder(im, ti, exifcommon.Ifd1StandardIfdIdentity, byteOrder)
		err = ifd1.AddStandard(0x0103, []uint16{6})
		log.PanicIf(err)
		err = ifd1.SetThumbnail([]byte("THUMBNAIL-BYTES-0123456789"))
		log.PanicIf(err)
		err = rootIb.SetNextIb(ifd1)
		log.PanicIf(err)
	}

	ibe := NewIfdByteEncoder()

	rawExif, err := ibe.EncodeToExif(rootIb)
	log.PanicIf(err)

	return rawExif
}

// -------------------------------------------------------------------
// Precision
// -------------------------------------------------------------------

func TestSanitizeGps_TruncatePrecision_LittleEndian(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg") // II / little-endian

	original, _ := gpsCoordinateFromExif(t, rawExif)
	if math.Abs(original-26.5866666667) > 1e-6 {
		t.Fatalf("test premise wrong: original latitude (%.10f)", original)
	}

	strategy := GpsSanitizeStrategy{DropGpsIfd: false, DecimalPlaces: 2}

	sanitized, err := SanitizeGps(rawExif, strategy)
	log.PanicIf(err)

	eh, err := ParseExifHeader(sanitized)
	log.PanicIf(err)

	if eh.ByteOrder != binary.LittleEndian {
		t.Fatalf("byte-order was not preserved: %v", eh.ByteOrder)
	}

	lat, lon := gpsCoordinateFromExif(t, sanitized)

	// 26.5866... -> 26.58 ; -80.0536... -> -80.05
	if math.Abs(lat-26.58) > 1e-9 {
		t.Fatalf("latitude not truncated correctly: (%.10f)", lat)
	} else if math.Abs(lon-(-80.05)) > 1e-9 {
		t.Fatalf("longitude not truncated correctly: (%.10f)", lon)
	}

	// Sanitized value must never be more precise than requested.
	if math.Abs(lat-original) > 0.011 || math.Abs(lon-(-80.05361111)) > 0.011 {
		t.Fatalf("sanitized coordinate moved too far: lat=(%.10f) lon=(%.10f)", lat, lon)
	}
}

func TestSanitizeGps_TruncatePrecision_BigEndian(t *testing.T) {
	rawExif := buildSyntheticGpsExif(t, binary.BigEndian, false)

	latBefore, lonBefore := gpsCoordinateFromExif(t, rawExif)
	if math.Abs(latBefore-48.8569166667) > 1e-6 || math.Abs(lonBefore-2.2988611111) > 1e-6 {
		t.Fatalf("test premise wrong: lat=(%.10f) lon=(%.10f)", latBefore, lonBefore)
	}

	strategy := GpsSanitizeStrategy{DropGpsIfd: false, DecimalPlaces: 3}

	sanitized, err := SanitizeGps(rawExif, strategy)
	log.PanicIf(err)

	eh, err := ParseExifHeader(sanitized)
	log.PanicIf(err)

	if eh.ByteOrder != binary.BigEndian {
		t.Fatalf("byte-order was not preserved: %v", eh.ByteOrder)
	}

	lat, lon := gpsCoordinateFromExif(t, sanitized)

	if math.Abs(lat-48.856) > 1e-9 {
		t.Fatalf("latitude not truncated correctly: (%.10f)", lat)
	} else if math.Abs(lon-2.298) > 1e-9 {
		t.Fatalf("longitude not truncated correctly: (%.10f)", lon)
	}
}

func TestSanitizeGps_TruncatePrecision_ZeroAndSeven(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg")

	// Zero decimal places.
	sanitized, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 0})
	log.PanicIf(err)

	lat, lon := gpsCoordinateFromExif(t, sanitized)
	if lat != 26.0 {
		t.Fatalf("zero-precision latitude wrong: (%.10f)", lat)
	} else if lon != -80.0 {
		t.Fatalf("zero-precision longitude wrong: (%.10f)", lon)
	}

	// Maximum supported precision must succeed and preserve more detail.
	sanitized, err = SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: MaxGpsDecimalPlaces})
	log.PanicIf(err)

	lat, lon = gpsCoordinateFromExif(t, sanitized)
	if math.Abs(lat-26.5866666) > 1e-6 {
		t.Fatalf("max-precision latitude wrong: (%.10f)", lat)
	} else if math.Abs(lon-(-80.0536111)) > 1e-6 {
		t.Fatalf("max-precision longitude wrong: (%.10f)", lon)
	}
}

func TestSanitizeGps_InvalidPrecision(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg")

	for _, places := range []int{-1, MaxGpsDecimalPlaces + 1, 100} {
		_, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: places})
		if err == nil {
			t.Fatalf("expected error for precision (%d)", places)
		} else if log.Is(err, ErrGpsPrecisionInvalid) == false {
			t.Fatalf("expected ErrGpsPrecisionInvalid for (%d), got: %v", places, err)
		}
	}
}

// -------------------------------------------------------------------
// Drop
// -------------------------------------------------------------------

func TestSanitizeGps_DropGpsIfd(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg")

	if hasGpsIfd(t, rawExif) == false {
		t.Fatalf("test premise wrong: sample should have a GPS IFD")
	}

	sanitized, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DropGpsIfd: true})
	log.PanicIf(err)

	if hasGpsIfd(t, sanitized) == true {
		t.Fatalf("GPS IFD was not removed")
	}

	// The output must still parse cleanly through the regular reader.
	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	_, index, err := Collect(im, NewTagIndex(), sanitized)
	log.PanicIf(err)

	if len(index.Ifds) == 0 {
		t.Fatalf("all IFDs were removed, not just GPS")
	}

	// Camera/exposure tags survive.
	foundMake := false
	err = index.RootIfd.EnumerateTagsRecursively(func(ifd *Ifd, ite *IfdTagEntry) error {
		if ite.TagId() == 0x010f {
			foundMake = true
		}
		return nil
	})
	log.PanicIf(err)

	if foundMake == false {
		t.Fatalf("Make tag did not survive GPS removal")
	}
}

// -------------------------------------------------------------------
// No GPS / structural cases
// -------------------------------------------------------------------

func TestSanitizeGps_NoGps(t *testing.T) {
	rawExif := getExifSimpleTestIbBytes()

	sanitized, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected ErrNoGpsIfd")
	} else if log.Is(err, ErrNoGpsIfd) == false {
		t.Fatalf("expected ErrNoGpsIfd, got: %v", err)
	}

	if bytes.Compare(sanitized, rawExif) != 0 {
		t.Fatalf("GPS-less input should be returned unchanged")
	}
}

func TestSanitizeGps_MissingCoordinateComponent(t *testing.T) {
	// Build a GPS IFD with latitude but no longitude.
	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()
	byteOrder := binary.BigEndian

	rootIb := NewIfdBuilder(im, ti, exifcommon.IfdStandardIfdIdentity, byteOrder)

	gpsIb := NewIfdBuilder(im, ti, exifcommon.IfdGpsInfoStandardIfdIdentity, byteOrder)
	err = gpsIb.AddStandard(TagGpsVersionId, []byte{2, 3, 0, 0})
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLatitudeRefId, "N")
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLatitudeId, []exifcommon.Rational{
		{Numerator: 48, Denominator: 1}, {Numerator: 51, Denominator: 1}, {Numerator: 24, Denominator: 1},
	})
	log.PanicIf(err)
	err = rootIb.AddChildIb(gpsIb)
	log.PanicIf(err)

	ibe := NewIfdByteEncoder()
	rawExif, err := ibe.EncodeToExif(rootIb)
	log.PanicIf(err)

	_, err = SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected error for missing longitude")
	} else if log.Is(err, ErrGpsCoordinateMissing) == false {
		t.Fatalf("expected ErrGpsCoordinateMissing, got: %v", err)
	}
}

func TestSanitizeGps_ZeroDenominatorRational(t *testing.T) {
	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()
	byteOrder := binary.BigEndian

	rootIb := NewIfdBuilder(im, ti, exifcommon.IfdStandardIfdIdentity, byteOrder)

	gpsIb := NewIfdBuilder(im, ti, exifcommon.IfdGpsInfoStandardIfdIdentity, byteOrder)
	err = gpsIb.AddStandard(TagLatitudeRefId, "N")
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLatitudeId, []exifcommon.Rational{
		{Numerator: 1, Denominator: 0},
		{Numerator: 0, Denominator: 1},
		{Numerator: 0, Denominator: 1},
	})
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLongitudeRefId, "E")
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLongitudeId, []exifcommon.Rational{
		{Numerator: 1, Denominator: 1},
		{Numerator: 0, Denominator: 1},
		{Numerator: 0, Denominator: 1},
	})
	log.PanicIf(err)
	err = rootIb.AddChildIb(gpsIb)
	log.PanicIf(err)

	rawExif, err := NewIfdByteEncoder().EncodeToExif(rootIb)
	log.PanicIf(err)

	_, err = SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected error for zero denominator")
	} else if log.Is(err, ErrGpsCoordinateMissing) == false {
		t.Fatalf("expected ErrGpsCoordinateMissing, got: %v", err)
	}
}

func TestSanitizeGps_MalformedCoordinateTags(t *testing.T) {
	// GPSLatitude encoded with the wrong type (LONG instead of RATIONAL) must
	// be rejected with coordinate-specific context rather than a generic
	// failure.
	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()
	byteOrder := binary.BigEndian

	rootIb := NewIfdBuilder(im, ti, exifcommon.IfdStandardIfdIdentity, byteOrder)

	gpsIb := NewIfdBuilder(im, ti, exifcommon.IfdGpsInfoStandardIfdIdentity, byteOrder)
	err = gpsIb.AddStandard(TagLatitudeRefId, "N")
	log.PanicIf(err)

	wrongTypeBt := NewBuilderTag(
		exifcommon.IfdGpsInfoStandardIfdIdentity.UnindexedString(),
		TagLatitudeId,
		exifcommon.TypeLong,
		NewIfdBuilderTagValueFromBytes([]byte{0, 0, 0, 1}),
		byteOrder)
	err = gpsIb.Add(wrongTypeBt)
	log.PanicIf(err)

	err = gpsIb.AddStandard(TagLongitudeRefId, "E")
	log.PanicIf(err)
	err = gpsIb.AddStandard(TagLongitudeId, []exifcommon.Rational{
		{Numerator: 1, Denominator: 1}, {Numerator: 0, Denominator: 1}, {Numerator: 0, Denominator: 1},
	})
	log.PanicIf(err)
	err = rootIb.AddChildIb(gpsIb)
	log.PanicIf(err)

	rawExif, err := NewIfdByteEncoder().EncodeToExif(rootIb)
	log.PanicIf(err)

	_, err = SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected error for wrong coordinate tag type")
	} else if log.Is(err, ErrGpsCoordinateMissing) == false {
		t.Fatalf("expected ErrGpsCoordinateMissing, got: %v", err)
	}
}

func TestSanitizeGps_EmptyInputAndBadHeader(t *testing.T) {
	if _, err := SanitizeGps(nil, GpsSanitizeStrategy{DecimalPlaces: 2}); err == nil {
		t.Fatalf("expected error for empty input")
	} else if log.Is(err, ErrIfdStructureInvalid) == false {
		t.Fatalf("expected ErrIfdStructureInvalid for empty input, got: %v", err)
	}

	if _, err := SanitizeGps([]byte("not an exif block at all"), GpsSanitizeStrategy{DecimalPlaces: 2}); err == nil {
		t.Fatalf("expected error for unparseable header")
	} else if log.Is(err, ErrNoExif) == false {
		t.Fatalf("expected ErrNoExif for garbage header, got: %v", err)
	}
}

func TestSanitizeGps_ReferenceTagsPreserved(t *testing.T) {
	// The N/S and E/W reference tags must not be rewritten.
	rawExif := getGpsSampleExif(t, "gps.jpg")

	sanitized, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	log.PanicIf(err)

	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	_, index, err := Collect(im, NewTagIndex(), sanitized)
	log.PanicIf(err)

	gpsIfd, err := index.RootIfd.ChildWithIfdPath(exifcommon.IfdGpsInfoStandardIfdIdentity)
	log.PanicIf(err)

	latRef, err := gpsIfd.FindTagWithId(TagLatitudeRefId)
	log.PanicIf(err)

	latRefValue, err := latRef[0].Value()
	log.PanicIf(err)

	if latRefValue != "N" {
		t.Fatalf("latitude reference changed: [%v]", latRefValue)
	}

	lonRef, err := gpsIfd.FindTagWithId(TagLongitudeRefId)
	log.PanicIf(err)

	lonRefValue, err := lonRef[0].Value()
	log.PanicIf(err)

	if lonRefValue != "W" {
		t.Fatalf("longitude reference changed: [%v]", lonRefValue)
	}
}

func TestSanitizeGps_GpsIfdWithoutCoordinates(t *testing.T) {
	// A GPS IFD that holds only a version tag (the Canon sample's shape) has
	// no precise location and should pass through untouched.
	im, err := exifcommon.NewIfdMappingWithStandard()
	log.PanicIf(err)

	ti := NewTagIndex()
	byteOrder := binary.LittleEndian

	rootIb := NewIfdBuilder(im, ti, exifcommon.IfdStandardIfdIdentity, byteOrder)
	err = rootIb.AddStandard(0x010f, "AcmeCam")
	log.PanicIf(err)

	gpsIb := NewIfdBuilder(im, ti, exifcommon.IfdGpsInfoStandardIfdIdentity, byteOrder)
	err = gpsIb.AddStandard(TagGpsVersionId, []byte{2, 3, 0, 0})
	log.PanicIf(err)
	err = rootIb.AddChildIb(gpsIb)
	log.PanicIf(err)

	ibe := NewIfdByteEncoder()
	rawExif, err := ibe.EncodeToExif(rootIb)
	log.PanicIf(err)

	sanitized, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	log.PanicIf(err)

	if hasGpsIfd(t, sanitized) == false {
		t.Fatalf("coordinate-less GPS IFD should be retained")
	}

	if reflect.DeepEqual(collectTags(t, rawExif), collectTags(t, sanitized)) == false {
		t.Fatalf("coordinate-less GPS IFD should be re-encoded identically")
	}
}

func TestSanitizeGps_BadOffset(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg")

	// Corrupt the first-IFD offset in the header (bytes 4..8) to point past
	// the end of the block.
	corrupt := make([]byte, len(rawExif))
	copy(corrupt, rawExif)
	binary.LittleEndian.PutUint32(corrupt[4:8], uint32(len(rawExif)+200))

	_, err := SanitizeGps(corrupt, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected error for bad first-IFD offset")
	} else if log.Is(err, ErrIfdStructureInvalid) == false {
		t.Fatalf("expected ErrIfdStructureInvalid, got: %v", err)
	}

	// A truncated block whose header offset points at unreadable data.
	truncated := make([]byte, len(rawExif))
	copy(truncated, rawExif)
	truncated = truncated[:10]

	_, err = SanitizeGps(truncated, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected error for truncated EXIF block")
	} else if log.Is(err, ErrIfdStructureInvalid) == false && log.Is(err, ErrNoExif) == false {
		t.Fatalf("expected structural error for truncated block, got: %v", err)
	}
}

// patchGpsIfdOffset rewrites the GPSTag (0x8825) pointer inside IFD0 to the
// given offset. The pointer is always a four-byte LONG located in an IFD
// entry; we scan IFD0 entries directly.
func patchGpsIfdOffset(t *testing.T, rawExif []byte, newOffset uint32) []byte {
	t.Helper()

	eh, err := ParseExifHeader(rawExif)
	log.PanicIf(err)

	out := make([]byte, len(rawExif))
	copy(out, rawExif)

	ifdOffset := int(eh.FirstIfdOffset)
	count := int(eh.ByteOrder.Uint16(out[ifdOffset : ifdOffset+2]))

	for i := 0; i < count; i++ {
		entry := ifdOffset + 2 + i*12
		tagId := eh.ByteOrder.Uint16(out[entry : entry+2])
		if tagId == 0x8825 {
			eh.ByteOrder.PutUint32(out[entry+8:entry+12], newOffset)
			return out
		}
	}

	t.Fatalf("GPSTag entry not found while patching offset")
	return nil
}

func TestSanitizeGps_BadGpsIfdOffset(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg")

	corrupt := patchGpsIfdOffset(t, rawExif, uint32(len(rawExif)+500))

	_, err := SanitizeGps(corrupt, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected error for unreachable GPS IFD offset")
	} else if log.Is(err, ErrIfdStructureInvalid) == false {
		t.Fatalf("expected ErrIfdStructureInvalid, got: %v", err)
	}
}

func TestSanitizeGps_MutuallyReferencingIfds(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg")

	eh, err := ParseExifHeader(rawExif)
	log.PanicIf(err)

	// Point the GPSTag at the root IFD itself, producing a cycle.
	corrupt := patchGpsIfdOffset(t, rawExif, eh.FirstIfdOffset)

	_, err = SanitizeGps(corrupt, GpsSanitizeStrategy{DecimalPlaces: 2})
	if err == nil {
		t.Fatalf("expected error for cyclic IFD references")
	} else if log.Is(err, ErrIfdStructureInvalid) == false {
		t.Fatalf("expected ErrIfdStructureInvalid, got: %v", err)
	}
}

// -------------------------------------------------------------------
// Preservation: other tags, unknown/private tags, thumbnail, immutability
// -------------------------------------------------------------------

func TestSanitizeGps_PreservesOtherTagsAndPrivateTags(t *testing.T) {
	rawExif := buildSyntheticGpsExif(t, binary.BigEndian, false)

	before := collectTags(t, rawExif)

	sanitized, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	log.PanicIf(err)

	after := collectTags(t, sanitized)

	// Everything except the four coordinate tags must be identical.
	filter := func(lines []string) []string {
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			if isCoordinateTagLine(line) {
				continue
			}
			out = append(out, line)
		}
		sort.Strings(out)
		return out
	}

	beforeFiltered := filter(before)
	afterFiltered := filter(after)

	if reflect.DeepEqual(beforeFiltered, afterFiltered) == false {
		t.Fatalf("non-coordinate tags changed:\nBEFORE: %v\nAFTER:  %v", beforeFiltered, afterFiltered)
	}

	// Explicitly confirm the private MakerNote survived byte-for-byte.
	var foundPrivate bool
	for _, line := range afterFiltered {
		if line == fmt.Sprintf("IFD/Exif|0x927c|%s|8|deadbeef00112233", exifcommon.TypeUndefined) {
			foundPrivate = true
		}
	}

	if foundPrivate == false {
		t.Fatalf("private MakerNote tag was not preserved: %v", afterFiltered)
	}
}

func TestSanitizeGps_PreservesCanonMakerNote(t *testing.T) {
	// The real Canon sample carries an 8KB MakerNote of vendor-private bytes.
	assetsPath := exifcommon.GetTestAssetsPath()
	rawExif, err := ioutil.ReadFile(path.Join(assetsPath, "NDM_8901.jpg.exif"))
	log.PanicIf(err)

	makerBefore := func() []byte {
		im, err := exifcommon.NewIfdMappingWithStandard()
		log.PanicIf(err)

		_, index, err := Collect(im, NewTagIndex(), rawExif)
		log.PanicIf(err)

		var raw []byte
		err = index.RootIfd.EnumerateTagsRecursively(func(ifd *Ifd, ite *IfdTagEntry) error {
			if ite.IfdPath() == "IFD/Exif" && ite.TagId() == 0x927c {
				raw, err = ite.GetRawBytes()
				log.PanicIf(err)
			}
			return nil
		})
		log.PanicIf(err)

		return raw
	}()

	if len(makerBefore) == 0 {
		t.Fatalf("test premise wrong: Canon MakerNote not found")
	}

	for _, strategy := range []GpsSanitizeStrategy{
		{DecimalPlaces: 2},
		{DropGpsIfd: true},
	} {
		sanitized, err := SanitizeGps(rawExif, strategy)
		log.PanicIf(err)

		im, err := exifcommon.NewIfdMappingWithStandard()
		log.PanicIf(err)

		_, index, err := Collect(im, NewTagIndex(), sanitized)
		log.PanicIf(err)

		var makerAfter []byte
		err = index.RootIfd.EnumerateTagsRecursively(func(ifd *Ifd, ite *IfdTagEntry) error {
			if ite.IfdPath() == "IFD/Exif" && ite.TagId() == 0x927c {
				makerAfter, err = ite.GetRawBytes()
				log.PanicIf(err)
			}
			return nil
		})
		log.PanicIf(err)

		if bytes.Compare(makerBefore, makerAfter) != 0 {
			t.Fatalf("Canon MakerNote changed under strategy %+v: (%d) != (%d) bytes",
				strategy, len(makerBefore), len(makerAfter))
		}
	}
}

func TestSanitizeGps_PreservesThumbnail(t *testing.T) {
	rawExif := buildSyntheticGpsExif(t, binary.LittleEndian, true)

	beforeThumbnail := getThumbnailBytes(t, rawExif)
	if beforeThumbnail == nil {
		t.Fatalf("test premise wrong: thumbnail missing")
	}

	sanitized, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 3})
	log.PanicIf(err)

	afterThumbnail := getThumbnailBytes(t, sanitized)
	if bytes.Compare(beforeThumbnail, afterThumbnail) != 0 {
		t.Fatalf("thumbnail bytes were not preserved")
	}

	// Also verify against the real Canon sample, whose IFD1 carries a large
	// thumbnail.
	assetsPath := exifcommon.GetTestAssetsPath()
	canonExif, err := ioutil.ReadFile(path.Join(assetsPath, "NDM_8901.jpg.exif"))
	log.PanicIf(err)

	// The Canon sample has GPS; truncate coordinates and re-check.
	sanitizedCanon, err := SanitizeGps(canonExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	log.PanicIf(err)

	if bytes.Compare(getThumbnailBytes(t, canonExif), getThumbnailBytes(t, sanitizedCanon)) != 0 {
		t.Fatalf("Canon thumbnail bytes were not preserved")
	}
}

func TestSanitizeGps_DoesNotModifyInput(t *testing.T) {
	rawExif := getGpsSampleExif(t, "gps.jpg")

	snapshot := make([]byte, len(rawExif))
	copy(snapshot, rawExif)

	_, err := SanitizeGps(rawExif, GpsSanitizeStrategy{DecimalPlaces: 2})
	log.PanicIf(err)

	if bytes.Compare(rawExif, snapshot) != 0 {
		t.Fatalf("SanitizeGps modified the input slice")
	}

	_, err = SanitizeGps(rawExif, GpsSanitizeStrategy{DropGpsIfd: true})
	log.PanicIf(err)

	if bytes.Compare(rawExif, snapshot) != 0 {
		t.Fatalf("SanitizeGps (drop mode) modified the input slice")
	}
}

func TestSanitizeGps_OutputReparsesAndRoundTrips(t *testing.T) {
	// Both modes, both byte orders: the output must parse again and a second
	// sanitization pass must be idempotent on the coordinates.
	for _, byteOrder := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		rawExif := buildSyntheticGpsExif(t, byteOrder, true)

		for _, strategy := range []GpsSanitizeStrategy{
			{DecimalPlaces: 3},
			{DropGpsIfd: true},
		} {
			first, err := SanitizeGps(rawExif, strategy)
			log.PanicIf(err)

			// Reparse with the package's standard entry point.
			_, _, err = GetFlatExifData(first, nil)
			log.PanicIf(err)

			if strategy.DropGpsIfd == false {
				lat1, lon1 := gpsCoordinateFromExif(t, first)

				second, err := SanitizeGps(first, strategy)
				log.PanicIf(err)

				lat2, lon2 := gpsCoordinateFromExif(t, second)
				if lat1 != lat2 || lon1 != lon2 {
					t.Fatalf("coordinates not stable across repeated sanitization: (%v,%v) != (%v,%v)",
						lat1, lon1, lat2, lon2)
				}
			}
		}
	}
}

// -------------------------------------------------------------------
// DMS conversion unit tests
// -------------------------------------------------------------------

func TestDecimalToDmsRationals(t *testing.T) {
	cases := []struct {
		decimal       float64
		decimalPlaces int
		expected      []exifcommon.Rational
	}{
		{
			// 26.5866... truncated to 26.58 -> 26d34'48"
			decimal: 26.5866666667, decimalPlaces: 2,
			expected: []exifcommon.Rational{
				{Numerator: 26, Denominator: 1},
				{Numerator: 34, Denominator: 1},
				{Numerator: 48, Denominator: 1},
			},
		},
		{
			// 48.856916... truncated to 48.856 -> 48d51'21.6" (216/10 -> 108/5)
			decimal: 48.8569166667, decimalPlaces: 3,
			expected: []exifcommon.Rational{
				{Numerator: 48, Denominator: 1},
				{Numerator: 51, Denominator: 1},
				{Numerator: 108, Denominator: 5},
			},
		},
		{
			// -80.0536... truncated (magnitude) to 80.05 -> 80d3'0"
			decimal: -80.0536111111, decimalPlaces: 2,
			expected: []exifcommon.Rational{
				{Numerator: 80, Denominator: 1},
				{Numerator: 3, Denominator: 1},
				{Numerator: 0, Denominator: 1},
			},
		},
		{
			decimal: 0, decimalPlaces: 4,
			expected: []exifcommon.Rational{
				{Numerator: 0, Denominator: 1},
				{Numerator: 0, Denominator: 1},
				{Numerator: 0, Denominator: 1},
			},
		},
	}

	for i, c := range cases {
		actual, err := decimalToDmsRationals(c.decimal, c.decimalPlaces)
		log.PanicIf(err)

		if reflect.DeepEqual(actual, c.expected) == false {
			t.Fatalf("case (%d) wrong: got %v expected %v", i, actual, c.expected)
		}

		// The reconstructed decimal must not exceed the requested precision.
		back := float64(actual[0].Numerator)/float64(actual[0].Denominator) +
			float64(actual[1].Numerator)/float64(actual[1].Denominator)/60.0 +
			(float64(actual[2].Numerator)/float64(actual[2].Denominator))/3600.0

		scaled := back * math.Pow10(c.decimalPlaces)
		if math.Abs(scaled-math.Trunc(scaled)) > 1e-6 {
			t.Fatalf("case (%d) result has excess precision: %.12f", i, back)
		}
	}
}
