# A Shiny app whose memory grows when you ask it to, for watching the MEM
# column of `shinyctl status`.
#
# The page shows which R process is serving it and how much it holds. The
# buttons add 100 MB or 1 GB, grow by 100 MB per second until stopped or
# until MAX_MB (set in the manifest's env), free it all, or crash the
# process. Nothing grows on start. The memory belongs to the R process, not to
# the browser tab: closing the tab stops the growth but keeps what is held,
# until "Free it" or a restart. The manifest's max-memory kills the process
# once it holds more than that, which is what "Grow until stopped" is for: the
# page drops, reloads on the warm copy, and starts again from 0 MB.
#
# "Compressible" is repeated values, which the macOS compressor packs well.
# "Incompressible" is random numbers, which is what forces real swap.

library(shiny)

# Top-level code runs once per R process, not once per browser session, so
# this is when the current process started.
started_at <- Sys.time()
max_mb <- as.integer(Sys.getenv("MAX_MB", "1024"))
step_mb <- 100

# Process-level, not per session: every tab on this copy sees one total.
heap <- new.env()
heap$chunks <- list()
heap$mb <- 0

# allocate adds mb MB of touched memory: rep() writes every element and
# runif() every random draw, so the pages are really dirtied; neither is a
# compact ALTREP sequence like 1:n, which takes no memory at all.
allocate <- function(mb, kind) {
  if (heap$mb + mb > max_mb) return(FALSE)
  n <- mb * 1024^2 / 8
  x <- if (kind == "rand") runif(n) else rep(c(1, 2), length.out = n)
  heap$chunks[[length(heap$chunks) + 1]] <- x
  heap$mb <- heap$mb + mb
  message(sprintf("holding %d MB", heap$mb))
  TRUE
}

ui <- fluidPage(
  # Reloads the page once the app answers again after its R process died,
  # instead of leaving it greyed out. A reload, not Shiny's own reconnect:
  # with shiny 1.14, a page reconnected to a new process keeps showing its
  # old outputs, and its download links stop working. Skipped in the first
  # 3 seconds after a load, so an app that fails at start isn't reloaded
  # over and over.
  tags$script(HTML(
    "$(document).on('shiny:disconnected', function poll() {
       if (performance.now() < 3000) return;
       fetch(location.href, {cache: 'no-store'})
         .then(r => r.ok ? location.reload() : Promise.reject())
         .catch(() => setTimeout(poll, 500));
     });"
  )),
  titlePanel("shinyctl memory example"),
  p(textOutput("process")),
  h4(textOutput("held")),
  radioButtons(
    "kind", NULL,
    c("Compressible (repeated values)" = "comp",
      "Incompressible (random numbers)" = "rand"),
    inline = TRUE
  ),
  actionButton("add100", "Add 100 MB"),
  actionButton("add1g", "Add 1 GB"),
  actionButton("grow", "Grow until stopped"),
  actionButton("free", "Free it"),
  actionButton("crash", "Crash this app", class = "btn-danger"),
  p("Growth stops at MAX_MB, set in the manifest's env. What is held belongs",
    "to this R process: closing the tab stops the growth but frees nothing.")
)

server <- function(input, output, session) {
  output$process <- renderText(sprintf(
    "Served by R process %d, started at %s.",
    Sys.getpid(), format(started_at, "%H:%M:%S")
  ))

  # Every tab shows the copy's real total, so it refreshes on its own.
  output$held <- renderText({
    invalidateLater(1000)
    sprintf("Holding %d MB of %d MB (R reports %.0f MB in use).",
            heap$mb, max_mb, gc()[2, 2])
  })

  # An allocation that fails ("cannot allocate vector") must not end the
  # session, and must not also report the ceiling.
  add <- function(mb, kind) {
    ok <- tryCatch(allocate(mb, kind), error = function(e) {
      showNotification(conditionMessage(e), type = "error")
      NA
    })
    if (is.na(ok)) return(FALSE)
    if (!ok) showNotification(sprintf("MAX_MB (%d) reached", max_mb))
    ok
  }

  observeEvent(input$add100, add(step_mb, input$kind))
  observeEvent(input$add1g, {
    for (i in 1:10) if (!add(step_mb, input$kind)) break
  })

  # Owned by the session, so closing the tab stops it.
  growing <- reactiveVal(FALSE)
  observeEvent(input$grow, {
    growing(!growing())
    updateActionButton(session, "grow",
                       label = if (growing()) "Stop" else "Grow until stopped")
  })
  observe({
    if (!growing()) return()
    # isolate: picking another kind must not run an extra step.
    if (add(step_mb, isolate(input$kind))) {
      invalidateLater(1000)
    } else {
      growing(FALSE)
      updateActionButton(session, "grow", label = "Grow until stopped")
    }
  })

  observeEvent(input$free, {
    heap$chunks <- list()
    heap$mb <- 0
    invisible(gc(full = TRUE))
    message("freed")
  })

  observeEvent(input$crash, {
    message("Crash button clicked, exiting with status 1")
    quit(save = "no", status = 1)
  })
}

shinyApp(ui, server)
