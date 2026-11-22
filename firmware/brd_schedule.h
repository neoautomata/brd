#pragma once

#include <algorithm>
#include <cstdint>
#include <cstdlib>
#include <ctime>
#include <mutex>
#include <string>

namespace brd_schedule {

struct WallTime {
  int year{0};
  int month{0};
  int day{0};
  int hour{0};
  int minute{0};
  int second{0};
};

using LocalTime = WallTime;

// Return true when year is a Gregorian leap year.
constexpr bool is_leap_year(uint16_t year) {
  return (year % 4U == 0U) && ((year % 100U != 0U) || (year % 400U == 0U));
}

// Return days in the given month and year, or 0 for an invalid month.
constexpr uint8_t days_in_month(uint16_t year, uint8_t month) {
  constexpr uint8_t DAYS[12] = {31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31};
  if (month < 1 || month > 12) {
    return 0;
  }
  if (month == 2 && is_leap_year(year)) {
    return 29;
  }
  return DAYS[month - 1];
}

// Normalize civil date and time fields with carry and borrow across boundaries.
inline bool normalize_civil_datetime(LocalTime &lt) {
  auto floor_div_mod = [](int64_t val, int64_t base, int &rem_out) -> int64_t {
    int64_t q = val / base;
    int64_t r = val % base;
    if (r < 0) {
      r += base;
      --q;
    }
    rem_out = static_cast<int>(r);
    return q;
  };

  int sec = 0;
  const int64_t min_carry = floor_div_mod(lt.second, 60, sec);
  int min = 0;
  const int64_t hour_carry = floor_div_mod(static_cast<int64_t>(lt.minute) + min_carry, 60, min);
  int hour = 0;
  const int64_t day_carry = floor_div_mod(static_cast<int64_t>(lt.hour) + hour_carry, 24, hour);

  int month_zero = 0;
  const int64_t year_carry = floor_div_mod(static_cast<int64_t>(lt.month) - 1, 12, month_zero);
  int month = month_zero + 1;
  int64_t year = static_cast<int64_t>(lt.year) + year_carry;
  if (year < 1 || year > 9999) {
    return false;
  }

  int64_t day = static_cast<int64_t>(lt.day) + day_carry;
  while (true) {
    const uint8_t dim = days_in_month(static_cast<uint16_t>(year), static_cast<uint8_t>(month));
    if (dim == 0 || day <= static_cast<int64_t>(dim)) {
      break;
    }
    day -= static_cast<int64_t>(dim);
    ++month;
    if (month > 12) {
      month = 1;
      ++year;
      if (year > 9999) {
        return false;
      }
    }
  }
  while (day < 1) {
    --month;
    if (month < 1) {
      month = 12;
      --year;
      if (year < 1) {
        return false;
      }
    }
    day += static_cast<int64_t>(days_in_month(static_cast<uint16_t>(year), static_cast<uint8_t>(month)));
  }

  lt.year = static_cast<int>(year);
  lt.month = month;
  lt.day = static_cast<int>(day);
  lt.hour = hour;
  lt.minute = min;
  lt.second = sec;
  return true;
}

class CalendarPort {
 public:
  virtual ~CalendarPort() = default;
  virtual bool epoch_to_local(int64_t epoch, WallTime &out) const = 0;
  virtual bool local_to_epoch(const WallTime &in, int64_t &epoch) const = 0;
  virtual bool epoch_to_utc(int64_t epoch, WallTime &out) const = 0;
};

class PosixTzCalendar : public CalendarPort {
 public:
  explicit PosixTzCalendar(const char *tz = "UTC")
      : tz_(tz != nullptr ? tz : "UTC") {}

  static std::mutex &tz_mutex() {
    static std::mutex m;
    return m;
  }

  bool epoch_to_local(int64_t epoch, WallTime &out) const override {
    const time_t t = static_cast<time_t>(epoch);
    std::lock_guard<std::mutex> lock(tz_mutex());
    if (!this->tz_.empty()) {
      const char *cur = std::getenv("TZ");
      if (cur == nullptr || this->tz_ != cur) {
        setenv("TZ", this->tz_.c_str(), 1);
        tzset();
      }
    }
    struct tm tm_buf {};
    if (localtime_r(&t, &tm_buf) == nullptr) {
      return false;
    }
    out.year = tm_buf.tm_year + 1900;
    out.month = tm_buf.tm_mon + 1;
    out.day = tm_buf.tm_mday;
    out.hour = tm_buf.tm_hour;
    out.minute = tm_buf.tm_min;
    out.second = tm_buf.tm_sec;
    return true;
  }

  bool local_to_epoch(const WallTime &in, int64_t &epoch) const override {
    struct tm tm_buf {};
    tm_buf.tm_year = in.year - 1900;
    tm_buf.tm_mon = in.month - 1;
    tm_buf.tm_mday = in.day;
    tm_buf.tm_hour = in.hour;
    tm_buf.tm_min = in.minute;
    tm_buf.tm_sec = in.second;
    tm_buf.tm_isdst = -1;
    std::lock_guard<std::mutex> lock(tz_mutex());
    if (!this->tz_.empty()) {
      const char *cur = std::getenv("TZ");
      if (cur == nullptr || this->tz_ != cur) {
        setenv("TZ", this->tz_.c_str(), 1);
        tzset();
      }
    }
    const time_t res = mktime(&tm_buf);
    if (res == static_cast<time_t>(-1)) {
      return false;
    }
    epoch = static_cast<int64_t>(res);
    return true;
  }

  bool epoch_to_utc(int64_t epoch, WallTime &out) const override {
    const time_t t = static_cast<time_t>(epoch);
    struct tm tm_buf {};
    if (gmtime_r(&t, &tm_buf) == nullptr) {
      return false;
    }
    out.year = tm_buf.tm_year + 1900;
    out.month = tm_buf.tm_mon + 1;
    out.day = tm_buf.tm_mday;
    out.hour = tm_buf.tm_hour;
    out.minute = tm_buf.tm_min;
    out.second = tm_buf.tm_sec;
    return true;
  }

 private:
  std::string tz_;
};

struct ScheduleConfig {
  int wake_hour{7};
  int wake_minute{0};
  int sleep_hour{22};
  int sleep_minute{0};
  int lead_margin_sec{120};
  int boundary_interval_min{15};
  int min_alarm_lead_sec{90};
  int bedtime_advance_threshold_sec{60};
  uint32_t fallback_sleep_sec{900};
};

struct SchedulePlan {
  const char *mode{"timer"};
  int64_t target_epoch{0};
  uint32_t sleep_seconds{900};
  uint8_t target_utc_hour{0};
  uint8_t target_utc_minute{0};
  bool clock_valid{false};
  bool arm_alarm{false};
};

inline SchedulePlan plan_next_wake(const CalendarPort &cal, int64_t now_epoch,
                                   const ScheduleConfig &cfg = {}) {
  SchedulePlan plan;
  plan.mode = "timer";
  plan.target_epoch = 0;
  plan.sleep_seconds = cfg.fallback_sleep_sec;
  plan.clock_valid = false;
  plan.arm_alarm = false;

  if (now_epoch <= 0) {
    return plan;
  }

  WallTime now_local {};
  if (!cal.epoch_to_local(now_epoch, now_local) || now_local.year < 2020) {
    return plan;
  }

  const int now_sec = now_local.hour * 3600 + now_local.minute * 60 + now_local.second;
  const int wake_sec = cfg.wake_hour * 3600 + cfg.wake_minute * 60;
  const int sleep_sec = cfg.sleep_hour * 3600 + cfg.sleep_minute * 60;

  WallTime bedtime_local = now_local;
  bedtime_local.hour = cfg.sleep_hour;
  bedtime_local.minute = cfg.sleep_minute;
  bedtime_local.second = 0;
  int64_t bedtime_epoch = 0;
  if (!cal.local_to_epoch(bedtime_local, bedtime_epoch)) {
    return plan;
  }

  int64_t target_epoch = 0;

  if (now_sec >= wake_sec && now_sec < sleep_sec) {
    plan.mode = "timer";

    // Find next interval boundary with lead margin.
    const int interval_min = cfg.boundary_interval_min > 0 ? cfg.boundary_interval_min : 15;
    const int next_min = ((now_local.minute / interval_min) + 1) * interval_min;
    WallTime cand = now_local;
    cand.second = 0;
    cand.minute = next_min % 60;
    cand.hour = now_local.hour + (next_min / 60);

    int64_t cand_epoch = 0;
    if (!cal.local_to_epoch(cand, cand_epoch)) {
      return plan;
    }

    if (cand_epoch - now_epoch < cfg.lead_margin_sec) {
      cand.minute += interval_min;
      if (cand.minute >= 60) {
        cand.minute -= 60;
        cand.hour += 1;
      }
      if (!cal.local_to_epoch(cand, cand_epoch)) {
        return plan;
      }
    }

    if (cand_epoch >= bedtime_epoch) {
      if (bedtime_epoch - now_epoch >= cfg.bedtime_advance_threshold_sec) {
        target_epoch = bedtime_epoch;
        plan.mode = "timer";
      } else {
        // Advance to next morning wake time.
        plan.mode = "alarm";
        WallTime next_day {};
        if (!cal.epoch_to_local(bedtime_epoch + 86400, next_day)) {
          return plan;
        }
        next_day.hour = cfg.wake_hour;
        next_day.minute = cfg.wake_minute;
        next_day.second = 0;
        if (!cal.local_to_epoch(next_day, target_epoch)) {
          return plan;
        }
      }
    } else {
      target_epoch = cand_epoch;
    }
  } else {
    plan.mode = "alarm";

    WallTime wake_local = now_local;
    wake_local.hour = cfg.wake_hour;
    wake_local.minute = cfg.wake_minute;
    wake_local.second = 0;

    int64_t wake_epoch = 0;
    if (!cal.local_to_epoch(wake_local, wake_epoch)) {
      return plan;
    }

    if (wake_epoch <= now_epoch) {
      WallTime next_day {};
      if (!cal.epoch_to_local(wake_epoch + 86400, next_day)) {
        return plan;
      }
      next_day.hour = cfg.wake_hour;
      next_day.minute = cfg.wake_minute;
      next_day.second = 0;
      if (!cal.local_to_epoch(next_day, target_epoch)) {
        return plan;
      }
    } else {
      target_epoch = wake_epoch;
    }
  }

  const int64_t diff = target_epoch - now_epoch;
  plan.target_epoch = target_epoch;
  plan.sleep_seconds = static_cast<uint32_t>(diff > 0 ? diff : 0);
  plan.clock_valid = true;
  plan.arm_alarm = false;

  if (diff >= cfg.min_alarm_lead_sec && diff < 86400) {
    WallTime target_utc {};
    if (cal.epoch_to_utc(target_epoch, target_utc)) {
      plan.arm_alarm = true;
      plan.target_utc_hour = static_cast<uint8_t>(target_utc.hour);
      plan.target_utc_minute = static_cast<uint8_t>(target_utc.minute);
    }
  }

  return plan;
}

inline SchedulePlan revalidate_before_sleep(const CalendarPort &cal, int64_t now_epoch,
                                            const SchedulePlan &plan,
                                            const ScheduleConfig &cfg = {}) {
  if (now_epoch <= 0) {
    SchedulePlan fallback;
    fallback.mode = plan.mode;
    fallback.target_epoch = 0;
    fallback.sleep_seconds = plan.sleep_seconds > 0 ? plan.sleep_seconds : cfg.fallback_sleep_sec;
    fallback.clock_valid = false;
    fallback.arm_alarm = false;
    return fallback;
  }

  if (!plan.clock_valid || plan.target_epoch == 0) {
    return plan_next_wake(cal, now_epoch, cfg);
  }

  const int64_t remaining = plan.target_epoch - now_epoch;
  if (remaining < cfg.min_alarm_lead_sec) {
    return plan_next_wake(cal, now_epoch, cfg);
  }

  SchedulePlan updated = plan;
  updated.sleep_seconds = static_cast<uint32_t>(remaining);
  updated.clock_valid = true;
  updated.arm_alarm = false;

  if (remaining >= cfg.min_alarm_lead_sec && remaining < 86400) {
    WallTime target_utc {};
    if (cal.epoch_to_utc(updated.target_epoch, target_utc)) {
      updated.arm_alarm = true;
      updated.target_utc_hour = static_cast<uint8_t>(target_utc.hour);
      updated.target_utc_minute = static_cast<uint8_t>(target_utc.minute);
    }
  }

  return updated;
}

}  // namespace brd_schedule

#if __has_include("esphome/core/time.h")
#if __has_include("esphome/core/defines.h")
#include "esphome/core/defines.h"
#endif
#include "esphome/core/time.h"

namespace brd_schedule {

// Calendar adapter that uses the parsed ESPHome global timezone.
class EspHomeTzCalendar : public CalendarPort {
 public:
  bool epoch_to_local(int64_t epoch, LocalTime &out) const override {
    if (epoch < 0) {
      return false;
    }
    const esphome::ESPTime t = esphome::ESPTime::from_epoch_local(static_cast<time_t>(epoch));
    if (!t.fields_in_range(true, true)) {
      return false;
    }
    out.year = t.year;
    out.month = t.month;
    out.day = t.day_of_month;
    out.hour = t.hour;
    out.minute = t.minute;
    out.second = t.second;
    return true;
  }

  bool local_to_epoch(const LocalTime &in, int64_t &epoch) const override {
    LocalTime norm = in;
    if (!normalize_civil_datetime(norm) || norm.year < 1970 || norm.year > 2999) {
      return false;
    }
    esphome::ESPTime t{};
    t.year = static_cast<uint16_t>(norm.year);
    t.month = static_cast<uint8_t>(norm.month);
    t.day_of_month = static_cast<uint8_t>(norm.day);
    t.hour = static_cast<uint8_t>(norm.hour);
    t.minute = static_cast<uint8_t>(norm.minute);
    t.second = static_cast<uint8_t>(norm.second);
    t.recalc_timestamp_local();
    if (t.timestamp == static_cast<time_t>(-1)) {
      return false;
    }
    epoch = static_cast<int64_t>(t.timestamp);
    return true;
  }

  bool epoch_to_utc(int64_t epoch, LocalTime &out) const override {
    const time_t t = static_cast<time_t>(epoch);
    struct tm tm_buf {};
    if (gmtime_r(&t, &tm_buf) == nullptr) {
      return false;
    }
    out.year = tm_buf.tm_year + 1900;
    out.month = tm_buf.tm_mon + 1;
    out.day = tm_buf.tm_mday;
    out.hour = tm_buf.tm_hour;
    out.minute = tm_buf.tm_min;
    out.second = tm_buf.tm_sec;
    return true;
  }
};

}  // namespace brd_schedule
#endif
