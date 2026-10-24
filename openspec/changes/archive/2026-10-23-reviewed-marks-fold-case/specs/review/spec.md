## ADDED Requirements

### Requirement: Reviewed marks ignore owner and repo case
A reviewed mark SHALL match its item whatever the case of the owner and repo it was typed with, since forge owner and repo names are case-insensitive. Marks already stored in another case MUST read as the same mark, the newest winning.

#### Scenario: Mark typed in another case
- **WHEN** `review mark CameronSjo/Forgectl#76` runs and the forge lists the item as `cameronsjo/forgectl#76`
- **THEN** the item is dimmed
- **AND** `review sync` with the item still open keeps the mark
