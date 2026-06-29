package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// PushupEntry holds one day of the 60-day pushup challenge
// (2026-07-01 .. 2026-08-29). Date is the natural primary key
// — one row per calendar day, regardless of submitter.
type PushupEntry struct {
	Date      time.Time  `json:"date" gorm:"type:date;primary_key"`
	Count     *int       `json:"count" gorm:"type:integer"`
	UpdatedAt time.Time  `json:"updated_at"`
	UpdatedBy uint       `json:"updated_by"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty" sql:"index"`
}

// PushupMilestone is the seeded cumulative-count → Swedish-message map.
// The challenge page joins the running cumulative against this table
// when an entry is saved; on a hit the message_sv string is surfaced.
type PushupMilestone struct {
	CumulativeCount int    `json:"cumulative_count" gorm:"primary_key"`
	MessageSv       string `json:"message_sv" gorm:"not null"`
}

func GetPushupEntries(db *gorm.DB) ([]PushupEntry, error) {
	var entries []PushupEntry
	err := db.Order("date asc").Find(&entries).Error
	return entries, err
}

func GetPushupEntryByDate(db *gorm.DB, date time.Time) (*PushupEntry, error) {
	var entry PushupEntry
	err := db.Where("date = ?", date).First(&entry).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func UpsertPushupEntry(db *gorm.DB, date time.Time, count *int, userID uint) (*PushupEntry, error) {
	existing, err := GetPushupEntryByDate(db, date)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if existing == nil {
		entry := PushupEntry{
			Date:      date,
			Count:     count,
			UpdatedAt: now,
			UpdatedBy: userID,
			CreatedAt: now,
		}
		if err := db.Create(&entry).Error; err != nil {
			return nil, err
		}
		return &entry, nil
	}
	existing.Count = count
	existing.UpdatedAt = now
	existing.UpdatedBy = userID
	if err := db.Save(existing).Error; err != nil {
		return nil, err
	}
	return existing, nil
}

func GetPushupMilestones(db *gorm.DB) ([]PushupMilestone, error) {
	var milestones []PushupMilestone
	err := db.Order("cumulative_count asc").Find(&milestones).Error
	return milestones, err
}

// pushupSeedMilestones is the canonical Swedish-message seed list.
// Roughly one entry every 10 cumulative reps across 1..1000, mixed
// form-cues, encouragement, numeric trivia, and a few Swedish nods.
// Operator-supplied specials at 59 / 100 / 200 / 500 / 555 / 1000.
// Overshoot zone (>1000) is rendered as "👑" in the frontend.
var pushupSeedMilestones = []PushupMilestone{
	{CumulativeCount: 10, MessageSv: "Tio i kassan — bra start!"},
	{CumulativeCount: 20, MessageSv: "20 — andra växeln i."},
	{CumulativeCount: 30, MessageSv: "30 — fortsätt andas."},
	{CumulativeCount: 40, MessageSv: "40 — rak rygg!"},
	{CumulativeCount: 50, MessageSv: "Halvhundra! Lugnt tempo nu."},
	{CumulativeCount: 59, MessageSv: "Lika många armhävningar som din ålder — nice!"},
	{CumulativeCount: 60, MessageSv: "60 — en per sekund i en minut, snyggt!"},
	{CumulativeCount: 70, MessageSv: "70 — armbågar nära kroppen."},
	{CumulativeCount: 80, MessageSv: "80 — sätt blicken framåt."},
	{CumulativeCount: 90, MessageSv: "90 — snart trecifrigt!"},
	{CumulativeCount: 100, MessageSv: "100 nere, 900 kvar!"},
	{CumulativeCount: 110, MessageSv: "110 — bygg på lugnt."},
	{CumulativeCount: 120, MessageSv: "120 — två extra dussin."},
	{CumulativeCount: 130, MessageSv: "130 — spänn magen."},
	{CumulativeCount: 140, MessageSv: "140 — en gammal tweet i armhävningar."},
	{CumulativeCount: 150, MessageSv: "150 — du är igång på riktigt nu."},
	{CumulativeCount: 160, MessageSv: "160 — åtta dussin, ren matte."},
	{CumulativeCount: 170, MessageSv: "170 — andas djupare."},
	{CumulativeCount: 180, MessageSv: "180 — full vändning! Ny riktning."},
	{CumulativeCount: 190, MessageSv: "190 — snart 200."},
	{CumulativeCount: 200, MessageSv: "200 nere, 800 kvar!"},
	{CumulativeCount: 210, MessageSv: "210 — håll axlarna borta från öronen."},
	{CumulativeCount: 220, MessageSv: "220 — strömstark!"},
	{CumulativeCount: 230, MessageSv: "230 — peppen finns."},
	{CumulativeCount: 240, MessageSv: "240 — fyra timmar i minuter, du."},
	{CumulativeCount: 250, MessageSv: "250 — en fjärdedel klar!"},
	{CumulativeCount: 260, MessageSv: "260 — fortsätt, en i taget."},
	{CumulativeCount: 270, MessageSv: "270 — tre fjärdedelar av 360."},
	{CumulativeCount: 280, MessageSv: "280 — räkna långsamt."},
	{CumulativeCount: 290, MessageSv: "290 — tio till så är det 300."},
	{CumulativeCount: 300, MessageSv: "300 nere, 700 kvar — bra tempo!"},
	{CumulativeCount: 310, MessageSv: "310 — du är i din zon."},
	{CumulativeCount: 320, MessageSv: "320 — händerna under axlarna."},
	{CumulativeCount: 333, MessageSv: "333 — en tredjedel hemma."},
	{CumulativeCount: 340, MessageSv: "340 — andas in, andas ut."},
	{CumulativeCount: 350, MessageSv: "350 — en tredjedel + femtio."},
	{CumulativeCount: 360, MessageSv: "360 — hela varvet runt!"},
	{CumulativeCount: 370, MessageSv: "370 — knän raka."},
	{CumulativeCount: 380, MessageSv: "380 — nära Fibonacci 377."},
	{CumulativeCount: 390, MessageSv: "390 — snart 400."},
	{CumulativeCount: 400, MessageSv: "400 nere, 600 kvar!"},
	{CumulativeCount: 410, MessageSv: "410 — ett rep i taget."},
	{CumulativeCount: 420, MessageSv: "420 — Pippi hade hejat på."},
	{CumulativeCount: 430, MessageSv: "430 — fokus på vägen ner."},
	{CumulativeCount: 440, MessageSv: "440 Hz — kammarton A, ren stämning."},
	{CumulativeCount: 450, MessageSv: "450 — 50 till halvvägs."},
	{CumulativeCount: 460, MessageSv: "460 — andas djupt."},
	{CumulativeCount: 470, MessageSv: "470 — Robban hejar."},
	{CumulativeCount: 480, MessageSv: "480 — retro upplösning, framtida muskler."},
	{CumulativeCount: 490, MessageSv: "490 — tio till halvvägs!"},
	{CumulativeCount: 500, MessageSv: "500 — halvvägs!"},
	{CumulativeCount: 510, MessageSv: "510 — över halvvägs!"},
	{CumulativeCount: 520, MessageSv: "520 — bygg vidare."},
	{CumulativeCount: 530, MessageSv: "530 — fortsätt lugnt."},
	{CumulativeCount: 540, MessageSv: "540 — en och en halv full vändning."},
	{CumulativeCount: 555, MessageSv: "555 — ditt favoritnummer!"},
	{CumulativeCount: 560, MessageSv: "560 — fem mer bortom favoriten."},
	{CumulativeCount: 570, MessageSv: "570 — sjutton dussin, ren matte."},
	{CumulativeCount: 580, MessageSv: "580 — andra halvan rullar."},
	{CumulativeCount: 590, MessageSv: "590 — snart 600."},
	{CumulativeCount: 600, MessageSv: "600 nere, 400 kvar — nedförsbacke nu."},
	{CumulativeCount: 610, MessageSv: "610 — Fibonacci-tal, snyggt!"},
	{CumulativeCount: 620, MessageSv: "620 — andas in på vägen ner."},
	{CumulativeCount: 630, MessageSv: "630 — drygt 60% klart."},
	{CumulativeCount: 640, MessageSv: "640K är nog för någon — du tar mer."},
	{CumulativeCount: 650, MessageSv: "650 — 65% klart, fortsätt."},
	{CumulativeCount: 666, MessageSv: "666 — två tredjedelar klart!"},
	{CumulativeCount: 670, MessageSv: "670 — du har koll."},
	{CumulativeCount: 680, MessageSv: "680 — armbågar nära."},
	{CumulativeCount: 690, MessageSv: "690 — snart 700."},
	{CumulativeCount: 700, MessageSv: "700 nere, 300 kvar!"},
	{CumulativeCount: 710, MessageSv: "710 — bara 290 kvar."},
	{CumulativeCount: 720, MessageSv: "720p — du kör i HD."},
	{CumulativeCount: 730, MessageSv: "730 — 73% klart."},
	{CumulativeCount: 740, MessageSv: "740 — andas ut på vägen upp."},
	{CumulativeCount: 750, MessageSv: "750 — tre fjärdedelar!"},
	{CumulativeCount: 760, MessageSv: "760 — händerna stadigt."},
	{CumulativeCount: 770, MessageSv: "770 — ABBA: gimme one more rep."},
	{CumulativeCount: 780, MessageSv: "780 — rak nacke."},
	{CumulativeCount: 790, MessageSv: "790 — snart 800."},
	{CumulativeCount: 800, MessageSv: "800 nere, 200 kvar — slutspurt!"},
	{CumulativeCount: 810, MessageSv: "810 — sista femtedelen!"},
	{CumulativeCount: 820, MessageSv: "820 — du är nästan där."},
	{CumulativeCount: 830, MessageSv: "830 — 83% klart."},
	{CumulativeCount: 840, MessageSv: "840 — stadig hållning."},
	{CumulativeCount: 850, MessageSv: "850 — 150 kvar, fokus."},
	{CumulativeCount: 860, MessageSv: "860 — andas djupt."},
	{CumulativeCount: 870, MessageSv: "870 — håll i takten."},
	{CumulativeCount: 880, MessageSv: "880 — 88% klart."},
	{CumulativeCount: 890, MessageSv: "890 — snart sista hundran."},
	{CumulativeCount: 900, MessageSv: "900 nere, 100 kvar — sista hundran!"},
	{CumulativeCount: 910, MessageSv: "910 — nittio kvar!"},
	{CumulativeCount: 920, MessageSv: "920 — 92%, brinn."},
	{CumulativeCount: 930, MessageSv: "930 — 70 kvar."},
	{CumulativeCount: 940, MessageSv: "940 — sextio kvar, du fixar det."},
	{CumulativeCount: 950, MessageSv: "950 — 95%, sista delen."},
	{CumulativeCount: 960, MessageSv: "960 — fyrtio kvar."},
	{CumulativeCount: 970, MessageSv: "970 — trettio kvar!"},
	{CumulativeCount: 980, MessageSv: "980 — tjugo kvar."},
	{CumulativeCount: 990, MessageSv: "990 — tio kvar till mål!"},
	{CumulativeCount: 999, MessageSv: "999 — en enda till!"},
	{CumulativeCount: 1000, MessageSv: "Mål! Du klarade det!"},
}

// SeedPushupMilestones upserts the canonical list. Safe to call on every
// startup — existing rows keep their message_sv unless we re-add one with
// the same key (then it is overwritten with the in-code copy, which is
// the desired source-of-truth direction).
func SeedPushupMilestones(db *gorm.DB) {
	for _, m := range pushupSeedMilestones {
		var existing PushupMilestone
		err := db.Where("cumulative_count = ?", m.CumulativeCount).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			db.Create(&m)
			continue
		}
		if existing.MessageSv != m.MessageSv {
			db.Model(&existing).Update("message_sv", m.MessageSv)
		}
	}
}
