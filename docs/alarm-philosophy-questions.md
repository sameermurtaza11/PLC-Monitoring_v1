# Basic alarm management — decisions and open questions

This file lists every behaviour that the reference guide
(`ISA_18_2_Alarm_Management_Guide_Basic_Alarms.pdf`) does **not** specify, and
what the software currently does about it. The guide is an educational
implementation guide, not the standard; this software is **not** a claim of
ISA-18.2 compliance. Each item needs a decision in the site's **Alarm
Philosophy** and an engineering review. Status:

* **Applied (proposed)** — implemented as the project owner approved the plan; still to be confirmed by the Alarm Philosophy.
* **Open** — not implemented yet, or needs a value from the site.

## Alarm definitions and rationalization

| # | Question | Current behaviour | Status |
|---|---|---|---|
| 1 | Which edition of ISA-18.2 applies (2009, 2016, Version 2)? | Not assumed. The guide itself says to confirm it. | **Open** |
| 2 | Engineering criteria and response expectations for **Minor / Major / Critical** | Only the three labels exist (database CHECK). No criteria are invented. The site must write them down here. | **Open — site to fill** |
| 3 | Is priority derived from the alarm type? | No. Type (HI, HIHI, LO, LOLO, DISCRETE) and priority are separate fields; any combination is allowed. Priority must be justified in the `rationale` field. | Applied |
| 4 | Who approves a rationalization? | An alarm is evaluated only when it is enabled **and** its rationalization is `APPROVED` by an administrator. Rationale, operator action and consequences of inaction must be filled in first. | Applied (proposed) |
| 5 | Which changes withdraw an approval? | Setpoint, abnormal state, deadband, on/off-delay, priority, class, operator action, consequences, rationale. Enable/disable and wording changes keep it. | Applied (proposed) |
| 6 | What if a PV's unit or range changes after approval? | The alarm is flagged ("Issues") and not evaluated until reviewed. | Applied (proposed) |
| 7 | Alarm classes | The four classes of the guide: PROCESS, EQUIPMENT_PROTECTION, ENVIRONMENTAL, SAFETY_CRITICAL. | Applied |
| 8 | Maximum response time | Stored in seconds, kept separate from the on-delay. Not enforced or escalated. | Applied (proposed) |
| 9 | Existing `pv_metadata.alarm_low/high/alarm_state` | Untouched (the dashboard still uses them for its HIGH/LOW status). They are not the alarm system. | Applied |

## Occurrences and states (not specified by the guide)

| # | Question | Current behaviour | Status |
|---|---|---|---|
| 10 | Completed occurrences | Removed from `active_alarms` when normal and acknowledged; the full timeline stays in `alarm_history` (closing event has `new_state = NORM`). | Applied |
| 11 | An alarm activates again while an older occurrence is still `RTNUN` | A **new** occurrence is created (every activation is its own occurrence). The database allows only one occurrence per alarm whose process condition is abnormal. | Applied (proposed) |
| 12 | Do the history rows keep the setpoint and priority that applied? | Yes (`alarm_history.priority`, `.setpoint`), so a later configuration change cannot change the meaning of an old event. This goes beyond the columns listed in the guide. | Applied (proposed) |
| 13 | Shelving: which states can be shelved? | Only an existing outstanding occurrence (UNACK, ACKED, RTNUN). Shelving an alarm that is not active is not supported in Version 01. | Applied (proposed) |
| 14 | State after unshelving | Process condition still abnormal → `UNACK`; already normal → `RTNUN`. The operator sees it again. | Applied (proposed) |
| 15 | Process changes while an alarm is shelved | The same occurrence stays `SHLVD`; `process_condition_active` follows the process; informational history events (no state change) are written. | Applied (proposed) |
| 16 | Maximum shelving duration | Open. A shelve needs a reason and an expiry; the maximum is a site value. | **Open — site to set** |
| 17 | Who may acknowledge / shelve / configure / approve | Acknowledge: all roles. Shelve and configure: supervisor and admin. Approve rationalization: admin. Enforced on the server. | Applied (proposed) |

## Monitoring and records

| # | Question | Current behaviour | Status |
|---|---|---|---|
| 18 | Alarm-rate and standing-alarm limits for the statistics | Statistics will show counts only; no target values are assumed. | **Open — site to set** |
| 19 | Retention and archiving of `alarm_history` | The table is append-only (database triggers). Archiving needs a controlled maintenance procedure (disable the trigger as a database administrator) that the site must define. | **Open** |
| 20 | Stale or failed PV data | Never treated as normal: an active alarm stays active, no new alarm is raised, and the PV is shown as bad data. | Applied |
| 21 | Delay timers across an application restart | Outstanding alarms survive restarts (they are in the database). A running on-delay or off-delay is not stored and starts again after a restart. | Applied (proposed) |

## Implementation status

Step 1 (definitions, rationalization, audit, permissions, database state rules) is
implemented and tested. Engine, acknowledgement, shelving, outstanding list, history
log and statistics follow, in that order.
